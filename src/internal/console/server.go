// Package console 提供面板的 HTTP 接口（鉴权、会话、渠道清单）。
//
// 本包是 M0 的可运行边界：首次访问落到设置密码向导，登录后能拿到渠道清单，
// 六屏前端可挂载。网关与路由在后续里程碑接入。
package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"poolgate/internal/errs"
	"poolgate/internal/health"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// Options 是面板服务的配置。
type Options struct {
	Version  string
	BasePath string
	// ListenAddr 是进程**实际**监听的地址（形如 0.0.0.0:5014）。
	//
	// 必须由启动方传入而不是读设置：安装包的启动脚本会显式给 -addr（飞牛的端口
	// 由应用中心分配），此时设置里的 listen_port 不生效。面板要显示「当前实际
	// 监听」，客户端要填的地址也得按这个推 —— 否则复制到的地址连不上。
	ListenAddr string
	// ListenPinned 表示监听地址由启动参数显式给定（不是设置里那两项）。
	//
	// 关键区别：安装包的启动脚本**总是**显式给 -addr，此时面板里的「监听地址/
	// 端口」改了也不生效。不把这件事说清楚，用户改完端口看到「已保存」就会以为
	// 生效了 —— 我们宁可让他看到一句「以启动参数为准」。
	ListenPinned bool
	// Pool 账号池（供总览/账号接口）。可为 nil（测试）。
	Pool *pool.Pool
	// Log 请求流水（供日志/统计接口）。可为 nil（测试）。
	Log *store.RequestLog
	// Creds 凭证存储（渠道授权成功后落盘）。为 nil 时授权仍可走通但不落盘 ——
	// 生产装配必须给，否则重启后授权结果就丢了。
	Creds *store.CredsStore
	// Settings 面板设置（F6.4）。可为 nil（测试）。
	Settings *store.SettingsStore
	// Keys 网关 API Key（面板显示/轮换）。可为 nil（测试）。
	Keys *store.APIKeyStore
	// Providers API Key 式来源的管理（面板「接入源」页）。为 nil 时该页只读/不可用。
	Providers ProviderAdmin
	// Checkins 签到结果（F1.8）。可为 nil（测试）。
	Checkins *store.CheckinStore
	// Health 健康探测执行器（F5.1/F5.3）。可为 nil（测试）。
	Health *health.Runner
	// History 体检历史（F5.5）。可为 nil（测试）。
	History *health.History
	// Excluded 模型下发剔除表（F4.4）。可为 nil（测试）。
	Excluded *ModelExclusions
	// HealthyModels 返回「只下发可用模型」生效时的体检结论索引（F4.5）。
	// 由装配层用 health.Health 构造，与网关共用同一个函数。可为 nil（测试）。
	HealthyModels func() health.Snapshot
	// DataDir / CredsDir 仅用于设置屏展示路径。
	DataDir  string
	CredsDir string
	// ApplyOverrides 在渠道开关变化后重算注册表状态。
	ApplyOverrides func()
	// ImportCreds 从 wild-work 目录导入凭证（F6.6）。
	ImportCreds func(from string) (int, error)
}

// ModelExclusions 是「因健康度被剔除下发」的模型集合（F4.4）。
//
// 与 registry 的渠道级 Status 分开：渠道级是配置裁定，模型级是探测结果，
// 两者粒度不同，混在一起会让「渠道可用但某个模型挂死」无法表达。
type ModelExclusions struct {
	mu   sync.RWMutex
	set  map[string]bool
	path string
}

// NewModelExclusions 建立剔除表；path 非空时落盘（重启后仍生效）。
func NewModelExclusions(path string) *ModelExclusions {
	m := &ModelExclusions{set: map[string]bool{}, path: path}
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var ids []string
			if json.Unmarshal(raw, &ids) == nil {
				for _, id := range ids {
					m.set[id] = true
				}
			}
		}
	}
	return m
}

// Has 报告某模型（kind/model 形式）是否被剔除下发。
func (m *ModelExclusions) Has(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.set[id]
}

// Set 设置剔除状态并落盘。
func (m *ModelExclusions) Set(id string, excluded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if excluded {
		m.set[id] = true
	} else {
		delete(m.set, id)
	}
	m.persistLocked()
}

// List 返回全部被剔除的模型 ID。
func (m *ModelExclusions) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.set))
	for id := range m.set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (m *ModelExclusions) persistLocked() {
	if m.path == "" {
		return
	}
	ids := make([]string, 0, len(m.set))
	for id := range m.set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	raw, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, m.path)
	}
}

// Server 是面板服务。
type Server struct {
	opts  Options
	admin *store.AdminStore
	sess  *store.SessionStore
	login *loginManager
}

// New 建立一个面板服务。
func New(admin *store.AdminStore, sess *store.SessionStore, o Options) *Server {
	if o.Version == "" {
		o.Version = "dev"
	}
	s := &Server{opts: o, admin: admin, sess: sess}
	m := &loginManager{now: time.Now}
	if o.Creds != nil {
		m.save = o.Creds.Save
	}
	if o.Pool != nil {
		m.pool = o.Pool.AddFor
	}
	s.login = m
	return s
}

// Routes 返回面板路由。管理类接口一律要求会话（设计方案 §7）。
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/setup", s.handleSetup)
	mux.HandleFunc("/api/channels", s.withAuth(s.handleChannels))
	mux.HandleFunc("/api/overview", s.withAuth(s.handleOverview))
	mux.HandleFunc("/api/accounts", s.withAuth(s.handleAccounts))
	mux.HandleFunc("/api/logs", s.withAuth(s.handleLogs))
	mux.HandleFunc("/api/models", s.withAuth(s.handleModels))
	// 渠道授权（设备流/回调流）：控制台登录 ≠ 渠道授权，但授权操作同样要求已登录。
	mux.HandleFunc("/api/login/start", s.withAuth(s.handleLoginStart))
	mux.HandleFunc("/api/login/poll", s.withAuth(s.handleLoginPoll))
	mux.HandleFunc("/api/login/cancel", s.withAuth(s.handleLoginCancel))
	// 手工回填回调地址（本机回调被网络挡住时的兜底，如千问办公）
	mux.HandleFunc("/api/login/callback", s.withAuth(s.handleLoginCallback))
	// 账号操作（F1.7/F1.8/F1.9）
	mux.HandleFunc("/api/account/enable", s.withAuth(s.handleAccountEnable))
	mux.HandleFunc("/api/account/remove", s.withAuth(s.handleAccountRemove))
	mux.HandleFunc("/api/account/checkin", s.withAuth(s.handleAccountCheckin))
	mux.HandleFunc("/api/account/refresh", s.withAuth(s.handleAccountRefresh))
	mux.HandleFunc("/api/account/checkin_all", s.withAuth(s.handleCheckinAll))
	mux.HandleFunc("/api/account/refresh_all", s.withAuth(s.handleRefreshAll))
	// 接入源（API Key 式来源）：增删改查 + 连通性测试（含工具调用验证）
	mux.HandleFunc("/api/providers", s.withAuth(s.handleProviders))
	mux.HandleFunc("/api/providers/save", s.withAuth(s.handleProviderSave))
	mux.HandleFunc("/api/providers/delete", s.withAuth(s.handleProviderDelete))
	mux.HandleFunc("/api/providers/test", s.withAuth(s.handleProviderTest))
	// 模型与费率（F4.x）
	mux.HandleFunc("/api/model/catalog", s.withAuth(s.handleModelCatalog))
	mux.HandleFunc("/api/model/toggle", s.withAuth(s.handleModelToggle))
	// 体检与测速（F5.x）
	mux.HandleFunc("/api/probe", s.withAuth(s.handleProbe))
	mux.HandleFunc("/api/probe/history", s.withAuth(s.handleProbeHistory))
	// 诊断与日志（F6.1/F6.3）
	mux.HandleFunc("/api/diagnose", s.withAuth(s.handleDiagnose))
	mux.HandleFunc("/api/logs/filter", s.withAuth(s.handleLogsFiltered))
	mux.HandleFunc("/api/logs/clear", s.withAuth(s.handleLogsClear))
	mux.HandleFunc("/api/logs/export", s.withAuth(s.handleLogsExport))
	// 设置与运维（F6.4/F6.6）
	mux.HandleFunc("/api/settings", s.withAuth(s.handleSettings))
	// 管理员密码：面板内改密（改完注销其它会话）。走 withAuth —— 必须先是管理员。
	mux.HandleFunc("/api/admin/password", s.withAuth(s.handleChangePassword))
	// 网关 API Key：掩码常显，明文按需，轮换需确认（F3.5）。
	mux.HandleFunc("/api/apikey", s.withAuth(s.handleAPIKeyGet))
	mux.HandleFunc("/api/apikey/reveal", s.withAuth(s.handleAPIKeyReveal))
	mux.HandleFunc("/api/apikey/rotate", s.withAuth(s.handleAPIKeyRotate))
	mux.HandleFunc("/api/channel/toggle", s.withAuth(s.handleChannelToggle))
	mux.HandleFunc("/api/backup", s.withAuth(s.handleBackup))
	mux.HandleFunc("/api/migrate", s.withAuth(s.handleMigrateFromWildWork))
	mux.HandleFunc("/api/danger/reset", s.withAuth(s.handleDangerReset))
	mux.HandleFunc("/api/danger/remove_accounts", s.withAuth(s.handleDangerRemoveAccounts))
	return mux
}

// bootstrapResp 是前端打开后的第一个请求：决定显示登录页还是设置向导。
type bootstrapResp struct {
	Initialized bool   `json:"initialized"`
	Version     string `json:"version"`
	BasePath    string `json:"base_path"`
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 GET"))
		return
	}
	writeJSON(w, http.StatusOK, bootstrapResp{
		Initialized: s.admin.Exists(),
		Version:     s.opts.Version,
		BasePath:    s.opts.BasePath,
	})
}

type setupReq struct {
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

type sessionResp struct {
	OK        bool   `json:"ok"`
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// handleSetup 只在未初始化时可用；成功后永久关闭（§11.1）。
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "仅支持 POST"))
		return
	}
	if s.admin.Exists() {
		writeErr(w, http.StatusConflict, errs.New(errs.AuthFailed, "管理员密码已设置"))
		return
	}
	var req setupReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
		return
	}
	if req.Password == "" {
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "密码不能为空"))
		return
	}
	if req.Password != req.Confirm {
		// 失败必带原因：不说「请重试」，说明哪一条不满足。
		writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "两次输入的密码不一致"))
		return
	}
	// 密码强度只作提示，不作门槛：这是自用单管理员系统，锁的是自己。
	// 前端把结论显示成文字（原型 07-login.html 的 pw-note），后端不因此拒绝。
	if err := s.admin.Set(req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "保存管理员密码失败").WithCause(err))
		return
	}
	log.Printf("console: 管理员密码已设置，设置接口从此关闭")

	sess, err := s.sess.Create(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "签发会话失败").WithCause(err))
		return
	}
	setSessionCookie(w, sess)
	writeJSON(w, http.StatusOK, sessionResp{OK: true, Token: sess.Token, ExpiresAt: sess.ExpiresAt.Format(time.RFC3339)})
}

type loginReq struct {
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 未登录返回 401，前端据此跳转登录页。
		if !s.authed(r) {
			writeErr(w, http.StatusUnauthorized, errs.New(errs.AuthFailed, "未登录或会话已过期"))
			return
		}
		writeJSON(w, http.StatusOK, sessionResp{OK: true})

	case http.MethodPost:
		var req loginReq
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, errs.New(errs.Parse, "请求体无法解析").WithCause(err))
			return
		}
		ip := clientIP(r)
		if remain, locked := s.sess.CheckLock(ip); locked {
			// 锁定提示写明剩余时间，不只是「已锁定」。
			writeErr(w, http.StatusTooManyRequests, errs.New(errs.AuthFailed,
				"连续失败次数过多，已锁定；剩余 "+fmtDuration(remain)))
			return
		}
		ok, err := s.admin.Verify(req.Password)
		if err != nil {
			if errors.Is(err, store.ErrNotInitialized) {
				writeErr(w, http.StatusConflict, errs.New(errs.AuthFailed, "尚未设置管理员密码"))
				return
			}
			writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "校验失败").WithCause(err))
			return
		}
		if !ok {
			remain, locked := s.sess.RecordFailure(ip)
			msg := "密码错误"
			if locked {
				msg = "密码错误，已连续失败 5 次，锁定 15 分钟"
			} else {
				msg = fmt.Sprintf("密码错误，还可尝试 %d 次", remain)
			}
			log.Printf("console: 登录失败 ip=%s 剩余=%d locked=%v", ip, remain, locked)
			writeErr(w, http.StatusUnauthorized, errs.New(errs.AuthFailed, msg))
			return
		}
		s.sess.ClearFailures(ip)
		sess, err := s.sess.Create(req.Remember)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, errs.New(errs.Parse, "签发会话失败").WithCause(err))
			return
		}
		setSessionCookie(w, sess)
		writeJSON(w, http.StatusOK, sessionResp{OK: true, Token: sess.Token, ExpiresAt: sess.ExpiresAt.Format(time.RFC3339)})

	case http.MethodDelete:
		s.sess.Destroy(sessionToken(r))
		clearSessionCookie(w)
		writeJSON(w, http.StatusOK, sessionResp{OK: false})

	default:
		writeErr(w, http.StatusMethodNotAllowed, errs.New(errs.Parse, "不支持的方法"))
	}
}

func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Kind        string `json:"kind"`
		DisplayName string `json:"display_name"`
		Status      string `json:"status"`
		Downstream  bool   `json:"downstream"`
		// Implemented 表示该渠道有真实适配器（能对话）。
		Implemented bool `json:"implemented"`
		// PanelAuth 表示该渠道能在面板里走授权流程加号。
		// 与 Implemented 分开：千问办公/QoderCOM/WorkBuddyAI 有适配器但没实现
		// channel.Authorizer，面板上不能给「授权」按钮（点了没反应）。
		PanelAuth bool `json:"panel_auth"`
		// PanelAuthNote 说明不能面板授权时的替代做法。
		PanelAuthNote string `json:"panel_auth_note,omitempty"`
		// AccountCount 该渠道当前账号数（前端据此提示「先去加号」）。
		AccountCount int `json:"account_count"`
		// Source 区分来源类型：login（登录授权式）| api_key（API Key 式来源）。
		// 「接入源」页把两类并到一张表，靠它去重 —— 否则 key 式来源会被列出两次。
		Source string `json:"source"`
	}
	isKeySource := func(kind string) bool {
		if s.opts.Providers == nil {
			return false
		}
		for _, c := range s.opts.Providers.List() {
			if c.Name == kind {
				return true
			}
		}
		return false
	}
	out := []item{}
	for _, e := range registry.All() {
		_, impl := registry.Get(e.Spec.Kind)
		_, canAuth := authorizerFor(e.Spec.Kind)
		n := 0
		if s.opts.Pool != nil {
			n = len(s.opts.Pool.List(e.Spec.Kind))
		}
		note := ""
		if !canAuth {
			note = "该渠道暂不支持面板授权，请用「从 wild-work 导入账号」"
		}
		out = append(out, item{
			Kind:          string(e.Spec.Kind),
			DisplayName:   e.Spec.DisplayName,
			Status:        string(e.Spec.Status),
			Downstream:    e.Spec.Downstream(),
			Implemented:   impl,
			PanelAuth:     canAuth,
			PanelAuthNote: note,
			AccountCount:  n,
			Source:        map[bool]string{true: "api_key", false: "login"}[isKeySource(string(e.Spec.Kind))],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out})
}

// passwordStrength 返回 0–4 的强度分与文字结论。
// 只作提示，不作门槛 —— 调用方（前端 pw-note）把结论显示给用户，后端不据此拒绝。
// 原型 07-login.html 的首次设置视图把结论显示成文字，不只靠颜色进度条。
func passwordStrength(pw string) (int, string) {
	var hasUpper, hasLower, hasDigit, hasSym bool
	for _, c := range pw {
		switch {
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= '0' && c <= '9':
			hasDigit = true
		default:
			hasSym = true
		}
	}
	if len(pw) < 8 {
		return 0, "太短（建议至少 8 位，不影响保存）"
	}
	if len(pw) < 12 {
		return 1, "偏弱（建议 12 位以上，并混合大小写与符号）"
	}
	switch {
	case hasUpper && hasLower && (hasDigit || hasSym):
		return 3, "强（已满足 ≥12 位且含大小写与符号）"
	case hasUpper || hasLower:
		return 2, "中（建议再混入符号或数字）"
	default:
		return 1, "偏弱（建议混合大小写与符号）"
	}
}
