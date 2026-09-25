// settings.go 面板设置的持久化（需求 F6.4：监听、冷却策略、签到保活等可改）。
//
// 与 config.json（只放 API Key）分开存，避免「改设置」和「换 Key」互相踩。
// 文件权限 0600：设置里可能含对外基址等内网信息。
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ChannelOverride 是单渠道的人工开关（F2.3 / 设置屏「渠道开关」）。
// 它是配置级操作，不需要改代码也不需要重启。
type ChannelOverride struct {
	Status string `json:"status"` // active | paused
	Note   string `json:"note,omitempty"`
}

// Settings 是面板可改的运行设置。
//
// 只有明确「改了有意义」的项才放进来：监听地址这类需要重启进程的，
// 存下来供展示与下次启动读取，但不会在当前进程内偷偷生效（UI 会写明）。
type Settings struct {
	// 服务
	ListenHost string `json:"listen_host"`
	ListenPort int    `json:"listen_port"`
	BaseURL    string `json:"base_url"`

	// 路由与冷却
	StickyRequests int `json:"sticky_requests"`
	MaxRetry       int `json:"max_retry"`
	// CooldownSeconds 覆盖 errs.DefaultPolicy 的冷却时长（按 Kind 名索引）。
	// 只覆盖冷却时长，不改「是否禁用/是否换号」——那些是判据的一部分，不做成旋钮。
	CooldownSeconds map[string]int `json:"cooldown_seconds"`

	// 健康探测
	ProbeIntervalHours int `json:"probe_interval_hours"`
	ProbeSamples       int `json:"probe_samples"`
	ProbeTimeoutSec    int `json:"probe_timeout_sec"`
	// OnlyHealthyModels 打开后 `/v1/models` 不再下发「体检明确失败」的模型。
	//
	// 「未体检」的模型**照样下发**：刚装完或没跑过体检时把它们一起藏掉，
	// 客户端会拿到一个空列表 —— 那看起来像服务坏了，而不是「模型不健康」。
	// 只影响下发列表；直连调用不拦（探测失败可能是一次性的，调用失败本就带原因）。
	OnlyHealthyModels bool `json:"only_healthy_models"`

	// 签到与保活
	CheckinTimes      []string                   `json:"checkin_times"`
	KeepaliveHours    int                        `json:"keepalive_hours"`
	CheckinEnabled    bool                       `json:"checkin_enabled"`
	KeepaliveEnabled  bool                       `json:"keepalive_enabled"`
	ChannelOverrides  map[string]ChannelOverride `json:"channel_overrides"`
	LogRetentionDays  int                        `json:"log_retention_days"`
	LogRetentionMaxMB int                        `json:"log_retention_max_mb"`
	LastBackupAt      string                     `json:"last_backup_at,omitempty"`
}

// DefaultSettings 返回出厂设置。数值与原型 06-settings.html 一致。
func DefaultSettings() Settings {
	return Settings{
		ListenHost:         "0.0.0.0",
		ListenPort:         5014,
		BaseURL:            "",
		StickyRequests:     50,
		MaxRetry:           3,
		CooldownSeconds:    map[string]int{},
		ProbeIntervalHours: 6,
		ProbeSamples:       3,
		// 30s 对慢渠道（千问办公实测首字 11–16s）偏紧，默认给 60s。
		ProbeTimeoutSec: 60,
		// 默认关：升级上来的机器行为不变，用户自己决定要不要过滤。
		OnlyHealthyModels: false,
		CheckinTimes:      []string{"09:00", "21:00"},
		KeepaliveHours:    22,
		CheckinEnabled:    true,
		KeepaliveEnabled:  true,
		ChannelOverrides:  map[string]ChannelOverride{},
		LogRetentionDays:  30,
		LogRetentionMaxMB: 200,
	}
}

// SettingsStore 是设置的读写入口。并发安全。
type SettingsStore struct {
	mu   sync.RWMutex
	path string
	cur  Settings
}

// NewSettingsStore 建立设置存储；文件不存在时用默认值（不写盘，等第一次保存）。
func NewSettingsStore(dir string) *SettingsStore {
	s := &SettingsStore{path: filepath.Join(dir, "settings.json"), cur: DefaultSettings()}
	if raw, err := os.ReadFile(s.path); err == nil {
		s.cur = decodeSettings(raw)
	}
	return s
}

// decodeSettings 以默认值为底，只覆盖文件里**确实出现过**的键。
//
// 不能直接 json.Unmarshal 到 Settings：那样文件里没写的字段会被零值覆盖
// （CheckinEnabled 之类 bool 的零值是 false，会把默认开启的签到悄悄关掉）。
func decodeSettings(raw []byte) Settings {
	def := DefaultSettings()
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		return def
	}
	var loaded Settings
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return def
	}
	apply := func(key string, dst any) {
		raw, ok := present[key]
		if !ok {
			return
		}
		// 注意是「把文件里的值解进 dst」，不是「把 dst 序列化再解回 dst」——
		// 后者等于没读文件，会让设置看起来保存了、重启后却全丢。
		_ = json.Unmarshal(raw, dst)
	}
	out := def
	apply("listen_host", &out.ListenHost)
	apply("listen_port", &out.ListenPort)
	apply("base_url", &out.BaseURL)
	apply("sticky_requests", &out.StickyRequests)
	apply("max_retry", &out.MaxRetry)
	apply("cooldown_seconds", &out.CooldownSeconds)
	apply("probe_interval_hours", &out.ProbeIntervalHours)
	apply("probe_samples", &out.ProbeSamples)
	apply("probe_timeout_sec", &out.ProbeTimeoutSec)
	apply("only_healthy_models", &out.OnlyHealthyModels)
	apply("checkin_times", &out.CheckinTimes)
	apply("keepalive_hours", &out.KeepaliveHours)
	apply("checkin_enabled", &out.CheckinEnabled)
	apply("keepalive_enabled", &out.KeepaliveEnabled)
	apply("channel_overrides", &out.ChannelOverrides)
	apply("log_retention_days", &out.LogRetentionDays)
	apply("log_retention_max_mb", &out.LogRetentionMaxMB)
	apply("last_backup_at", &out.LastBackupAt)
	if out.CooldownSeconds == nil {
		out.CooldownSeconds = map[string]int{}
	}
	if out.ChannelOverrides == nil {
		out.ChannelOverrides = map[string]ChannelOverride{}
	}
	return out
}

// Path 返回设置文件路径。
func (s *SettingsStore) Path() string { return s.path }

// Get 返回当前设置的副本（调用方可安全改动）。
func (s *SettingsStore) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyLocked()
}

func (s *SettingsStore) copyLocked() Settings {
	out := s.cur
	out.CheckinTimes = append([]string(nil), s.cur.CheckinTimes...)
	out.CooldownSeconds = map[string]int{}
	for k, v := range s.cur.CooldownSeconds {
		out.CooldownSeconds[k] = v
	}
	out.ChannelOverrides = map[string]ChannelOverride{}
	for k, v := range s.cur.ChannelOverrides {
		out.ChannelOverrides[k] = v
	}
	return out
}

// Update 用 fn 修改设置并落盘。fn 返回错误时不落盘、不改内存。
func (s *SettingsStore) Update(fn func(*Settings) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.copyLocked()
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.writeLocked(next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// SetChannelStatus 设置某渠道的人工开关（设置屏「渠道开关」）。
func (s *SettingsStore) SetChannelStatus(kind, status, note string) error {
	return s.Update(func(st *Settings) error {
		if st.ChannelOverrides == nil {
			st.ChannelOverrides = map[string]ChannelOverride{}
		}
		st.ChannelOverrides[kind] = ChannelOverride{Status: status, Note: note}
		return nil
	})
}

// MarkBackup 记录一次备份时间。
func (s *SettingsStore) MarkBackup() error {
	return s.Update(func(st *Settings) error {
		st.LastBackupAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	})
}

func (s *SettingsStore) writeLocked(next Settings) error {
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
