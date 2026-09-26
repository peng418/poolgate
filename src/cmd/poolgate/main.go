// Command poolgate 是 PoolGate · AI 账号池网关 的主程序。
//
// 交付形态：单二进制 + go:embed 静态资源（沿用 wild-work 的 FPK 路线）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"poolgate/internal/boot"
	"poolgate/internal/channel"
	"poolgate/internal/console"
	"poolgate/internal/errs"
	"poolgate/internal/gateway"
	"poolgate/internal/health"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/store"
	"poolgate/internal/webui"
)

// Version 由构建期注入：
//
//	go build -ldflags "-X main.Version=0.1.0"
var Version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	var (
		addr     = flag.String("addr", "0.0.0.0:5014", "面板与网关监听地址")
		dataDir  = flag.String("data", defaultDataDir(), "数据目录（配置、状态、流水）")
		confDir  = flag.String("conf", defaultConfDir(), "凭证目录（0600）")
		basePath = flag.String("base-path", "", "反向代理下的路径前缀")
	)
	flag.Parse()

	// -addr 是否由启动方显式给出。安装包的启动脚本一定会给（飞牛的端口由应用中心
	// 分配），此时设置里的 listen_host/port 不生效 —— 面板会把「当前实际监听」
	// 显示出来，避免用户改了端口却以为生效了。
	addrExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrExplicit = true
		}
	})

	if len(flag.Args()) > 0 {
		if err := runSubcommand(flag.Args(), *confDir, *dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "poolgate: %v\n", err)
			os.Exit(1)
		}
		return
	}

	boot.RegisterChannels()

	admin := store.NewAdminStore(*confDir)
	sess := store.NewSessionStore()

	// 账号池 + 凭证加载 + API Key + 请求流水
	accPool := pool.New()
	reqLog := store.NewRequestLog(500)
	creds := store.NewCredsStore(filepath.Join(*confDir, "creds"))
	settings := store.NewSettingsStore(*dataDir)
	// 没显式指定 -addr 时，让设置里的监听地址生效（改了端口重启就能用）。
	// 显式给了就以启动参数为准：那是安装/运维层面的裁定，不该被面板悄悄改掉。
	if !addrExplicit {
		if st := settings.Get(); st.ListenPort > 0 {
			host := strings.TrimSpace(st.ListenHost)
			if host == "" {
				host = "0.0.0.0"
			}
			*addr = net.JoinHostPort(host, strconv.Itoa(st.ListenPort))
		}
	}
	checkins := store.NewCheckinStore()
	exclusions := console.NewModelExclusions(filepath.Join(*dataDir, "excluded-models.json"))
	probeHistory := health.NewHistory(50)
	healthRunner := health.NewRunner(accPool, probeTimeout(settings))

	// 渠道开关（设置屏「渠道开关」）：把人工裁定叠到注册表上。
	applyOverrides(settings)

	// API Key 式来源（官方 API）：配置级接入 —— 填 base_url + key 就多一路。
	//
	// 与登录授权式渠道**共用同一个网关入口与账号池**（docs/02 §12）：客户端仍然只连
	// 一个地址、一个 key，模型名用来源前缀区分（如 google/gemini-3.8-flash）。
	// 这里给它合成一条「账号」：key 式来源没有多账号概念，但必须进池，否则路由层选不到号。
	providers := store.NewProviderStore(*dataDir)
	for _, p := range providers.List() {
		boot.MountProvider(p, accPool)
		log.Printf("poolgate: 接入源 %s（API Key 式）已挂载，base_url=%s，tools=%v",
			p.Name, p.BaseURL, p.SupportsTools)
	}

	// rebuildPool 重新扫描凭证目录 —— 授权成功/删除账号/导入后调用。
	rebuildPool := func() {}
	rebuildPool = func() {
		for _, e := range registry.Active() {
			// API Key 式来源的「账号」来自 providers.json（合成账号），不在凭证目录里。
			if _, isProvider := providers.Get(string(e.Spec.Kind)); isProvider {
				continue
			}
			cs, err := creds.Load(e.Spec.Kind)
			if err != nil {
				log.Printf("poolgate: 加载渠道 %s 凭证失败: %v", e.Spec.Kind, err)
				continue
			}
			for _, c := range cs {
				// COSY 系渠道（QoderCN / QoderCOM）需补齐机器指纹。
				boot.EnsureFingerprint(e.Spec.Kind, &c)
				accPool.AddFor(e.Spec.Kind, c)
			}
			log.Printf("poolgate: 渠道 %s 加载 %d 个账号", e.Spec.Kind, len(cs))
		}
	}
	rebuildPool()

	// API Key 要在面板之前建好：设置屏要显示/轮换它。
	keys := store.NewAPIKeyStore(*dataDir)
	if err := keys.Ensure(); err != nil {
		log.Printf("poolgate: 初始化 API Key 失败: %v", err)
	}

	// 凭证续期：适配器换 token → 落盘 → 回写池子。三步都做完才算成功 ——
	// refresh token 会被上游轮换，只换内存不落盘的话，重启后拿到的是已经作废的旧
	// refresh token，账号看起来「好好的」但再也刷新不出来（下次只能重新授权）。
	accPool.SetRefresher(func(ctx context.Context, kind channel.Kind, c channel.Credential) (*channel.Credential, error) {
		ch, ok := registry.Get(kind)
		if !ok {
			return nil, errs.New(errs.Parse, "渠道未实现，无法续期").WithChannel(string(kind))
		}
		nc, err := ch.Refresh(ctx, &c)
		if err != nil {
			// 续期失败必须留痕（红线一）：否则「续期链断了」会静默一小时以上才被发现
			// （2026-09-27 事故里就是这么查了半天：日志里只有成功的续期，失败的什么都没有）。
			log.Printf("poolgate: 凭证续期失败 %s/%s：%v", kind, c.UID, err)
			return nil, err
		}
		if nc == nil || nc.AccessToken == c.AccessToken {
			return nil, nil // 适配器表示刷不了（如无 refresh token）：交给 401 逻辑处理
		}
		if _, err := creds.Save(kind, *nc); err != nil {
			return nil, errs.New(errs.Parse, "凭证续期成功但落盘失败："+err.Error()).WithChannel(string(kind))
		}
		accPool.AddFor(kind, *nc)
		// 续期成功说明号本身是好的：把之前因「会话失效」被自动禁用的状态解掉。
		// 只在原因是会话失效时动手 —— 人工「停用」是管理员的决定，不能被自动续期推翻。
		if st, ok := accPool.Get(kind, nc.UID); ok && st.Disabled && credentialReason(st.Reason) {
			accPool.SetDisabled(kind, nc.UID, false)
			accPool.ClearCooldown(kind, nc.UID)
			log.Printf("poolgate: 凭证续期成功，账号已自动恢复启用 %s/%s", kind, nc.UID)
		}
		log.Printf("poolgate: 凭证已续期 %s/%s", kind, nc.UID)
		return nc, nil
	})

	// 续期节奏：声明了「一次性 refresh token」的渠道（channel.Spec.RefreshCadence）必须定期主动
	// 续期把链接着 —— 闲置的 rt 会失效（实测 ~31 小时）。节奏由渠道声明，装配层转给池子；
	// 池子据此决定「该不该续」，请求路径不再凭「上游给的到期提示」每请求都去敲一次。
	accPool.SetRefreshCadence(func(kind channel.Kind) time.Duration {
		if spec, ok := registry.GetSpec(kind); ok {
			return spec.RefreshCadence
		}
		return 0
	})

	// 续期心跳：请求路径只续**被选中**的号，闲置账号没人管（它的 rt 会放坏）。每分钟醒一次，
	// 真正续不续由池子按渠道节奏判定 —— 没声明节奏的渠道一次都不会续，不给上游白添调用。
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if n := accPool.RefreshSweep(ctx); n > 0 {
				log.Printf("poolgate: 续期心跳：%d 个账号到点，已尝试续期", n)
			}
			cancel()
		}
	}()

	// 「只下发可用模型」（F4.5）：开关在设置里，结论来自最近一次体检。在这里装一次，
	// 面板与网关共用同一个函数 —— 面板上标着「已按健康度隐藏」的模型，
	// 客户端那边一定也拿不到，不会出现两边说法不一致。
	healthyModels := health.Health(probeHistory, func() bool {
		return settings != nil && settings.Get().OnlyHealthyModels
	})

	srv := console.New(admin, sess, console.Options{
		Version:  Version,
		BasePath: *basePath,
		// 实际监听地址（可能已被设置覆盖）—— 面板据此显示真相并推客户端地址。
		ListenAddr:    *addr,
		ListenPinned:  addrExplicit,
		Pool:          accPool,
		Log:           reqLog,
		Creds:         creds,
		Settings:      settings,
		Keys:          keys,
		Checkins:      checkins,
		Providers:     providerAdmin{store: providers, pool: accPool},
		Health:        healthRunner,
		History:       probeHistory,
		Excluded:      exclusions,
		HealthyModels: healthyModels,
		DataDir:       *dataDir,
		CredsDir:      creds.Dir(),
		ApplyOverrides: func() {
			applyOverrides(settings)
			rebuildPool()
		},
		ImportCreds: func(from string) (int, error) {
			n, err := migrateFrom(from, *confDir)
			if err != nil {
				return n, err
			}
			// 导完立刻重建池子：凭证文件落盘了但池子没重建的话，面板上
			// 账号列不出来、余额也刷不了，用户会以为导入失败（得重启才生效）。
			if n > 0 {
				rebuildPool()
			}
			return n, nil
		},
	})
	// 全局出口代理：登录（还没有账号凭证）与对话都能走它。空 = 直连/环境变量。
	channel.SetEgressDefault(func() string {
		if settings == nil {
			return ""
		}
		return settings.Get().EgressProxy
	})

	// 每账号串行 + 最小间隔（防封号）：间隔按渠道可配（设置里的 account_min_interval_sec），
	// 缺省不限 —— 网页渠道应当设 1–3 秒，见 docs/06。
	accGate := pool.NewGate(func(kind channel.Kind) time.Duration {
		if settings == nil {
			return 0
		}
		if m := settings.Get().AccountMinIntervalSec; m != nil {
			if sec := m[string(kind)]; sec > 0 {
				return time.Duration(sec) * time.Second
			}
		}
		// 设置里没写：用渠道自己声明的出厂默认（网页渠道通常非 0 —— 见 docs/06）。
		if spec, ok := registry.GetSpec(kind); ok && spec.DefaultMinIntervalSec > 0 {
			return time.Duration(spec.DefaultMinIntervalSec) * time.Second
		}
		return 0
	})

	if settings != nil {
		if m := settings.Get().AccountMinIntervalSec; len(m) > 0 {
			log.Printf("poolgate: 每账号最小请求间隔（秒）: %v（防封号用，见 docs/06）", m)
		}
	}

	gw := gateway.New(accPool, keys, gateway.Options{
		BasePath: *basePath, Log: reqLog, Excluded: exclusions, Health: healthyModels,
		Gate: accGate,
	})

	// 启动时预热余额：余额只有问过上游才知道，不预热的话刚装完/刚重启的面板
	// 全是「未知」，看起来像没接上（F1.7）。异步跑，不阻塞监听。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if n := srv.RefreshAllBalances(ctx); n > 0 {
			log.Printf("poolgate: 已预热 %d 个账号的余额", n)
		}
	}()

	webHandler, err := webui.Handler()
	if err != nil {
		log.Fatalf("poolgate: 嵌入的静态资源不可用: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Routes())
	mux.Handle("/v1/", gw.Routes())
	mux.Handle("/", webHandler)

	log.Printf("poolgate %s 监听 %s", Version, *addr)
	log.Printf("数据目录 %s · 凭证目录 %s", *dataDir, *confDir)
	if !admin.Exists() {
		// 首次运行判据：密码文件不存在 → 打开面板会落到设置密码向导。
		log.Printf("尚未设置管理员密码，首次访问面板将进入设置向导（或用 poolgate admin set-password）")
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("poolgate: %v", err)
	}
}

// runSubcommand 处理 admin / migrate 类命令。忘记密码是唯一重置入口（无邮件、无找回）。
func runSubcommand(args []string, confDir, dataDir string) error {
	switch args[0] {
	case "admin":
		return runAdmin(args[1:], confDir)
	case "migrate":
		return runMigrate(args[1:], confDir)
	default:
		return fmt.Errorf("未知命令 %q", args[0])
	}
}

func runAdmin(args []string, confDir string) error {
	if len(args) < 2 {
		return fmt.Errorf("用法: poolgate admin <set-password|reset-password> <密码>")
	}
	admin := store.NewAdminStore(confDir)
	switch args[0] {
	case "set-password":
		if len(args) < 2 {
			return fmt.Errorf("用法: poolgate admin set-password <密码>")
		}
		if err := admin.Set(args[1]); err != nil {
			return err
		}
		fmt.Printf("管理员密码已设置 → %s\n", admin.Path())
		return nil
	case "reset-password":
		if len(args) < 2 {
			return fmt.Errorf("用法: poolgate admin reset-password <密码>")
		}
		if err := admin.Reset(args[1]); err != nil {
			return err
		}
		fmt.Printf("管理员密码已重置 → %s\n", admin.Path())
		return nil
	default:
		return fmt.Errorf("未知子命令 %q", args[0])
	}
}

// applyOverrides 把设置里的人工渠道开关叠到注册表上（设置屏「渠道开关」）。
//
// 注册表是进程级单例，这里直接改 Spec.Status：暂停的渠道保留账号与适配器，
// 但 Downstream() 变 false，于是 /v1/models 不再下发它的模型（F2.3）。
func applyOverrides(settings *store.SettingsStore) {
	if settings == nil {
		return
	}
	for kind, ov := range settings.Get().ChannelOverrides {
		switch ov.Status {
		case string(channel.Active):
			registry.SetStatus(channel.Kind(kind), channel.Active)
		case string(channel.Paused):
			registry.SetStatus(channel.Kind(kind), channel.Paused)
		}
	}
}

// credentialReason 报告「账号被自动禁用」的原因是不是凭证类问题。
//
// pool 在凭证失效时会把 Reason 记成错误分类（如 SessionDead）；
// 人工停用记的是「手动停用」。只有前者才允许被自动续期恢复。
func credentialReason(reason string) bool {
	return reason == string(errs.SessionDead) || reason == string(errs.AuthFailed)
}

// probeTimeout 读设置里的单模型超时。
func probeTimeout(settings *store.SettingsStore) time.Duration {
	if settings != nil {
		if s := settings.Get().ProbeTimeoutSec; s > 0 {
			return time.Duration(s) * time.Second
		}
	}
	return 30 * time.Second
}

// migrateFrom 从 wild-work 的 auth 目录导入凭证（F6.6）。
// 供 CLI 的 migrate 子命令与面板的服务端凭证导入共用同一实现。
func migrateFrom(from, confDir string) (int, error) {
	dst := filepath.Join(confDir, "creds")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return 0, err
	}
	files, err := filepath.Glob(filepath.Join(from, "*.json"))
	if err != nil {
		return 0, err
	}
	// 只导入首发三家渠道（channel 包里 Kind 常量）。qwenwork/qodercom/workbuddyai 按裁定暂停，不导入。
	importable := map[string]string{
		"qodercn":   "qodercn",
		"workbuddy": "workbuddy",
		"trae":      "traework", // wild-work 用 trae- 前缀
	}
	n := 0
	// 打不开的文件要把原因带回去：飞牛的跨应用目录是 0600 他人不可读，
	// 这时候面板只回一句「导入失败」等于没说话（红线一）——用户会以为是软件坏了。
	var skipped []string
	for _, f := range files {
		base := filepath.Base(f)
		dash := strings.IndexByte(base, '-')
		if dash <= 0 {
			continue
		}
		srcKind := base[:dash]
		dstKind, ok := importable[srcKind]
		if !ok {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Printf("migrate: 跳过 %s: %v", base, err)
			skipped = append(skipped, base+": "+err.Error())
			continue
		}
		// 校验能解析出 accessToken，坏文件不导入。
		var probe struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
			} `json:"auth"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil || probe.Auth.AccessToken == "" {
			log.Printf("migrate: 跳过 %s: 无法解析或缺少 accessToken", base)
			continue
		}
		target := filepath.Join(dst, dstKind+base[dash:])
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			return n, err
		}
		fmt.Printf("已导入 %s → %s\n", base, target)
		n++
	}
	if n == 0 {
		if len(skipped) > 0 {
			return 0, fmt.Errorf("源目录里的凭证都读不了（%d 个，例如 %s）；"+
				"跨应用目录默认 0600，需要以管理员身份把这些 json 复制到 %s 或放开读权限",
				len(skipped), skipped[0], dst)
		}
		return 0, fmt.Errorf("未发现可导入的凭证（检查 --from 目录）")
	}
	fmt.Printf("共导入 %d 个账号 → %s\n", n, dst)
	return n, nil
}

// runMigrate 是 CLI 的 migrate 子命令入口。
func runMigrate(args []string, confDir string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	from := fs.String("from", "", "wild-work auth 目录（含 <channel>-<uid>.json）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("用法: poolgate migrate --from <wild-work auth 目录>")
	}
	_, err := migrateFrom(*from, confDir)
	return err
}

func defaultDataDir() string {
	if v := os.Getenv("POOLGATE_DATA"); v != "" {
		return v
	}
	return filepath.Join("/vol6/@appdata", "poolgate")
}

func defaultConfDir() string {
	if v := os.Getenv("POOLGATE_CONF"); v != "" {
		return v
	}
	return filepath.Join("/vol6/@appconf", "poolgate")
}

// providerAdmin 把「接入源」的管理动作接到装配层：
// 落盘（store）+ 挂载/摘除（boot）+ 连通性测试（boot，真实打一次带 tools 的请求）。
type providerAdmin struct {
	store *store.ProviderStore
	pool  *pool.Pool
}

func (a providerAdmin) List() []store.ProviderConfig { return a.store.List() }

func (a providerAdmin) Put(cfg store.ProviderConfig) error {
	if err := a.store.Put(cfg); err != nil {
		return err
	}
	boot.MountProvider(cfg, a.pool) // 热更新：不重启就生效
	return nil
}

func (a providerAdmin) Remove(name string) error {
	if err := a.store.Remove(name); err != nil {
		return err
	}
	boot.UnmountProvider(name, a.pool)
	return nil
}

func (a providerAdmin) Test(ctx context.Context, cfg store.ProviderConfig) boot.ProbeResult {
	return boot.ProbeProvider(ctx, cfg)
}
