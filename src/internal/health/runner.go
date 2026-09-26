// runner.go 把健康判据接到真实上游：抽样体检 / 全量测速（需求 F5.1/F5.3/F5.4）。
//
// 关键约束（红线二）：探测必须发**真实请求**，且只认「收到了内容」。
// 这里不发 HEAD、不看状态码 —— health.Judge 是唯一判据。
//
// 本包不知道渠道协议：只通过 channel.Channel 接口发一次最小对话，
// 读第一个带内容的 chunk 作为「收到内容」的证据。
package health

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// RunKind 区分两种探测，面板的「体检历史」要写明类型（原型 04-benchmark.html）。
type RunKind string

const (
	RunSample RunKind = "sample" // 抽样体检：每渠道取代表模型，快
	RunFull   RunKind = "full"   // 全量测速：遍历全部模型
)

// Result 是一次探测的结论，直接对应面板表格的一行。
type Result struct {
	Channel   string    `json:"channel"`
	Account   string    `json:"account"`
	Model     string    `json:"model"`
	OK        bool      `json:"ok"`
	Kind      string    `json:"kind,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Upstream  string    `json:"upstream,omitempty"`
	TTFTms    int64     `json:"ttft_ms,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// Skipped 是这次探测里「因为账号自身的问题被跳过」的账号 uid（按跳过顺序）。
	// 有它面板才能说清「这条红是哪个号的锅」—— 只给一句「失败」等于让用户猜。
	Skipped []string `json:"skipped_accounts,omitempty"`
}

// Run 是一次探测批次的汇总（体检历史的一行）。
type Run struct {
	Kind      RunKind   `json:"kind"`
	StartedAt time.Time `json:"started_at"`
	Duration  int64     `json:"duration_ms"`
	Total     int       `json:"total"`
	Passed    int       `json:"passed"`
	Failed    int       `json:"failed"`
	Results   []Result  `json:"results"`
	Note      string    `json:"note,omitempty"`
}

// Target 是一条探测目标：某渠道的某个模型，用该渠道的某个账号探测。
type Target struct {
	Kind    channel.Kind
	Channel channel.Channel
	Account channel.Credential
	Model   string
}

// Entry 是一个渠道的探测素材，由调用方（console）从 registry 组装。
type Entry struct {
	Kind    channel.Kind
	Channel channel.Channel
	Models  []string
}

// Runner 执行健康探测。并发度固定为 3（需求 §5 非功能：测速 3 并发）。
type Runner struct {
	pool    AccountSource
	timeout time.Duration
	conc    int
}

// AccountSource 是 Runner 需要的账号视图。pool.Pool 实现之；测试可注入。
//
// 带 ctx 是有意的：选号会顺带做凭证续期（pool.Pick 的行为），
// 那是一次真实的上游请求，必须能随请求一起取消。
//
// 探测结论必须回写池子（NoteError / NoteSuccess）：体检发的是**真实请求**，
// 它的结论必须和真实流量共用同一份状态 —— 否则面板会对着一个死号说「渠道正常」，
// 也会对着活号说「渠道挂了」。
//
// 2026-09-26 的真机事故就是这么来的：池里躺着两个 token 早已失效的旧账号，
// 而 Pick 遍历的是 map、余额相同时顺序随机 —— 三个模型全被随机抽到死号，体检全红，
// 用户看到的是「上游原话：Authorization Failed (invalid token)」却不知道是哪个号。
type AccountSource interface {
	Pick(ctx context.Context, kind channel.Kind, tried map[string]bool) (channel.Credential, bool)
	// RefreshNow 强制续期一次（不看到期时间）。凭证类失败时先试它再换号，
	// 与 router.Route 的纪律一致 —— 直登账号能靠它救回来，而不是被直接禁用。
	RefreshNow(ctx context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error)
	// NoteError 按 errs.Kind 分档冷却/禁用该账号（UpstreamFault 等不计账号错误）。
	NoteError(kind channel.Kind, uid string, k errs.Kind)
	// NoteSuccess 清零该账号的错误计数。
	NoteSuccess(kind channel.Kind, uid string)
}

// NewRunner 建立探测器。timeout<=0 时用 30 秒（原型 06-settings.html 的默认）。
func NewRunner(p AccountSource, timeout time.Duration) *Runner {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Runner{pool: p, timeout: timeout, conc: 3}
}

// maxAccountAttempts 是一次探测最多用几个账号（含首个）。
//
// 与 router 的换号重试同一个道理：一条死号不该让整个渠道在面板上变红。
// 但不能无限换（池子可能有几十个号），所以给一个上限。
const maxAccountAttempts = 3

// ProbeOne 对单个目标发一次真实请求并判读。
//
// 这是判据的唯一执行点：先看请求是否发出，再看是否收到内容。
//
// 账号纪律（与 router.Route 一致，2026-09-26 补）：
//  1. 结论回写池子：失败按 errs.Kind 分档（NoteError），成功清零（NoteSuccess）；
//  2. 只有「账号/凭证的问题」才续期与换号 —— 上游故障、超长、内容拦截换号只会白折腾，
//     还会把一个好号冷却掉（红线 D4）；
//  3. 凭证类失败（SessionDead/AuthFailed）先强制续期一次再重试，救不回来才换下一个号；
//  4. 池里的号都试完了才认账「这个渠道现在不可用」，并把**最后一个真实错误**带出去。
func (r *Runner) ProbeOne(ctx context.Context, t Target) Result {
	cred := t.Account
	tried := map[string]bool{}
	var (
		skipped []string
		last    Result
	)

	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		tried[cred.UID] = true
		res, err := r.probeWith(ctx, t, cred)
		res.Skipped = append([]string(nil), skipped...)
		last = res
		if res.OK {
			r.pool.NoteSuccess(t.Kind, cred.UID)
			return res
		}
		k := failureKind(res, err)
		r.pool.NoteError(t.Kind, cred.UID, k)
		if !k.AccountBlamed() && !errs.CredentialKind(k) {
			// 不是这个号的锅：换号没意义，直接给出结论（渠道级故障）。
			return res
		}
		// 凭证类失败：先试续期再谈换号（「token 到期」唯一体面的处理方式）。
		if errs.CredentialKind(k) {
			if nc, rerr := r.pool.RefreshNow(ctx, t.Kind, cred); rerr == nil && nc != nil && nc.AccessToken != cred.AccessToken {
				res, err = r.probeWith(ctx, t, *nc)
				res.Skipped = append([]string(nil), skipped...)
				last = res
				if res.OK {
					r.pool.NoteSuccess(t.Kind, nc.UID)
					return res
				}
				k = failureKind(res, err)
				r.pool.NoteError(t.Kind, nc.UID, k)
				if !k.AccountBlamed() && !errs.CredentialKind(k) {
					return res
				}
			}
		}
		next, ok := r.pool.Pick(ctx, t.Kind, tried)
		if !ok {
			return last // 没有别的号可试了：这一条红才是真红
		}
		skipped = append(skipped, cred.UID)
		cred = next
	}
	return last
}

// probeWith 用指定账号发一次探测请求（换号/续期的纪律在 ProbeOne 外层）。
//
// 返回的 error 只在「请求本身就错了、没拿到流」或「读流中途断了」时非空；
// 「HTTP 200 但流里没内容」这类失败只体现在 Result 里（没有 error 可归因到账号）。
func (r *Runner) probeWith(ctx context.Context, t Target, cred channel.Credential) (Result, error) {
	res := Result{
		Channel:   string(t.Kind),
		Account:   cred.UID,
		Model:     t.Model,
		CheckedAt: time.Now().UTC(),
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	start := time.Now()
	stream, err := t.Channel.Chat(cctx, &cred, channel.ChatRequest{
		Model:     t.Model,
		Messages:  []channel.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 8,
		Stream:    true,
	})
	if err != nil {
		k, _ := errs.KindOf(err)
		v := Judge("", err, 0)
		res.Kind = string(k)
		res.Reason = v.Reason
		res.Upstream = upstreamOf(err)
		return res, err
	}
	defer stream.Close()

	// 读第一段带内容的 chunk —— 「收到首个内容」才算通过。
	var (
		sawContent bool
		ttft       time.Duration
		readErr    error
		sb         strings.Builder
	)
	for {
		chunk, err := stream.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		for _, c := range chunk.Choices {
			text := c.Delta.Content + c.Delta.ReasoningContent
			if text == "" {
				continue
			}
			if !sawContent {
				ttft = time.Since(start)
				sawContent = true
			}
			sb.WriteString(text)
			break
		}
		if sawContent {
			// 收到内容即判定，不必读完（省上游额度）。
			break
		}
	}

	v := Judge(sb.String(), readErr, ttft)
	res.OK = v.OK
	res.Kind = v.Kind
	res.Reason = v.Reason
	res.Upstream = v.Upstream
	if v.OK {
		res.TTFTms = v.TTFT.Milliseconds()
	}
	return res, readErr
}

// failureKind 取一次失败探测的分类：优先用错误自带的 Kind（带得最准），
// 其次用判据给的分类（空流那条路没有 error，判据会给出 UpstreamFault）。
func failureKind(res Result, err error) errs.Kind {
	if err != nil {
		if k, ok := errs.KindOf(err); ok {
			return k
		}
	}
	return errs.Kind(res.Kind)
}

// Sample 执行抽样体检：每渠道取 samples 个代表模型。
//
// 这是 F5.1 的落点：真实抽样请求「收到首个内容」才算通过，不看 HTTP 状态码。
func (r *Runner) Sample(ctx context.Context, entries []Entry, samples int) Run {
	if samples <= 0 {
		samples = 3
	}
	run := Run{Kind: RunSample, StartedAt: time.Now().UTC()}
	targets, missing := r.plan(ctx, entries, func(e Entry) []string {
		return pickRepresentative(e.Models, samples)
	})
	run.Results = append(run.Results, missing...)
	run.Results = append(run.Results, r.execute(ctx, targets)...)
	run.Duration = time.Since(run.StartedAt).Milliseconds()
	summarize(&run)
	return run
}

// Full 执行全量测速：遍历每个渠道的全部模型（原型 04-benchmark.html 的「全量测速」）。
func (r *Runner) Full(ctx context.Context, entries []Entry) Run {
	run := Run{Kind: RunFull, StartedAt: time.Now().UTC()}
	targets, missing := r.plan(ctx, entries, func(e Entry) []string { return e.Models })
	run.Results = append(run.Results, missing...)
	run.Results = append(run.Results, r.execute(ctx, targets)...)
	run.Duration = time.Since(run.StartedAt).Milliseconds()
	summarize(&run)
	return run
}

// plan 把渠道素材展开成探测目标；无可用账号的渠道单独记一条失败，
// 不让它从面板上悄悄消失（「不撒谎」的另一个侧面）。
func (r *Runner) plan(ctx context.Context, entries []Entry, modelsOf func(Entry) []string) ([]Target, []Result) {
	var (
		targets []Target
		missing []Result
	)
	for _, e := range entries {
		cred, ok := r.pool.Pick(ctx, e.Kind, nil)
		if !ok {
			missing = append(missing, Result{
				Channel: string(e.Kind), Model: "-", OK: false,
				Kind: string(errs.NoCandidate), Reason: "该渠道无可用账号，无法探测",
				CheckedAt: time.Now().UTC(),
			})
			continue
		}
		models := modelsOf(e)
		if len(models) == 0 {
			missing = append(missing, Result{
				Channel: string(e.Kind), Account: cred.UID, Model: "-", OK: false,
				Kind: string(errs.ModelUnavailable), Reason: "该渠道未返回任何模型，无法探测",
				CheckedAt: time.Now().UTC(),
			})
			continue
		}
		for _, m := range models {
			targets = append(targets, Target{Kind: e.Kind, Channel: e.Channel, Account: cred, Model: m})
		}
	}
	return targets, missing
}

// execute 以固定并发执行探测，并按 (渠道, 模型) 排序返回，保证面板顺序稳定。
func (r *Runner) execute(ctx context.Context, targets []Target) []Result {
	out := make([]Result, len(targets))
	sem := make(chan struct{}, r.conc)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = r.ProbeOne(ctx, t)
		}(i, t)
	}
	wg.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func summarize(run *Run) {
	run.Total = len(run.Results)
	run.Passed, run.Failed = 0, 0
	for _, r := range run.Results {
		if r.OK {
			run.Passed++
		} else {
			run.Failed++
		}
	}
}

// upstreamOf 取错误里的上游原话摘要（红线一：失败必带原因）。
func upstreamOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Upstream != "" {
		return e.Upstream
	}
	return truncate(err.Error(), 400)
}

// pickRepresentative 取代表模型：优先默认模型（auto / *-max），再按 ID 排序补齐。
func pickRepresentative(models []string, n int) []string {
	if len(models) == 0 {
		return nil
	}
	if n >= len(models) {
		return append([]string(nil), models...)
	}
	scored := append([]string(nil), models...)
	sort.SliceStable(scored, func(i, j int) bool {
		return scoreModel(scored[i]) > scoreModel(scored[j])
	})
	return scored[:n]
}

func scoreModel(id string) int {
	s := 0
	low := strings.ToLower(id)
	if strings.HasSuffix(low, "/auto") || low == "auto" {
		s += 100
	}
	if strings.Contains(low, "max") {
		s += 50
	}
	if strings.Contains(low, "pro") {
		s += 20
	}
	if strings.Contains(low, "flash") || strings.Contains(low, "mini") {
		s -= 20
	}
	return s
}

// History 保存最近若干次探测批次，供面板「体检历史」回看判据（F5.5）。
type History struct {
	mu   sync.Mutex
	runs []Run
	max  int
	// snap / snapValid 是 Snapshot() 的缓存（Add 之后失效）。见 snapshot.go。
	snap      Snapshot
	snapValid bool
}

// NewHistory 建立体检历史，保留最近 max 次。
func NewHistory(max int) *History {
	if max <= 0 {
		max = 20
	}
	return &History{max: max}
}

// Add 追加一次批次，超出上限丢弃最旧的。
func (h *History) Add(r Run) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapValid = false // 结论索引随批次失效，下次 Snapshot 重建
	h.runs = append(h.runs, r)
	if len(h.runs) > h.max {
		h.runs = h.runs[len(h.runs)-h.max:]
	}
}

// List 返回全部批次（时间正序）。
func (h *History) List() []Run {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Run, len(h.runs))
	copy(out, h.runs)
	return out
}

// Latest 返回最近一次批次。
func (h *History) Latest() (Run, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.runs) == 0 {
		return Run{}, false
	}
	return h.runs[len(h.runs)-1], true
}
