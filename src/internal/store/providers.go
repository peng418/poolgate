// providers.go 「API Key 式来源」（官方 API）的配置持久化。
//
// 与「登录授权式渠道」刻意分开：
//   - 登录式渠道的凭证是登录换来的 cookie/JWT，落 creds/ 目录，会过期、要续期、有账号概念；
//   - key 式来源的凭证就是用户填的一串 key，它是**配置**而不是会话。
//
// 所以单独存 providers.json（0600）：改它就是改配置，不需要授权流程，也没有「重新登录」。
// 面板「接入源」页把两类来源并到一张表里展示，但存储与生命周期各走各的。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProviderConfig 是一个 API Key 式来源。
type ProviderConfig struct {
	// Name 是模型前缀，同时也是 channel.Kind —— 客户端里会看到 <name>/<模型名>。
	// 一旦有客户端在用就不要改名（改了等于换了一个渠道）。
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	BaseURL     string `json:"base_url"` // 不含 /chat/completions
	APIKey      string `json:"api_key"`
	// Models 是手填的模型清单：上游没有 /v1/models 时（如 Perplexity）用它兜底。
	// 留空则完全依赖上游目录；两者都拿不到时**报错而不是给空列表**。
	Models []string `json:"models,omitempty"`
	// SupportsTools 声明该上游是否支持**原生**工具调用。声明 false 时按 ToolsMode 处理。
	SupportsTools bool `json:"supports_tools"`
	// ToolsMode 是工具调用的处理方式（三档）：
	//   "native" —— 上游原生支持，tools 原样透传；
	//   "shim"   —— 上游只会聊天，由**网关代做模拟**（提示词约定 + 解析回结构化 tool_calls）；
	//   "none"   —— 不带 tools 的纯聊天；
	// 留空时按 SupportsTools 推（true→native / false→none），兼容旧配置。
	ToolsMode string `json:"tools_mode,omitempty"`
	Notes     string `json:"notes,omitempty"`  // 免费额度 / 实名要求等提示（面板展示）
	Preset    string `json:"preset,omitempty"` // 来自哪个预设模板
	AddedAt   string `json:"added_at,omitempty"`
}

// builtinKinds 是六个内置登录式渠道的名字 —— key 式来源不能占用，否则会覆盖注册表里的实现。
var builtinKinds = map[string]bool{
	"qodercn": true, "qodercom": true, "traework": true,
	"workbuddy": true, "workbuddyai": true, "qwenwork": true,
}

// nameRe 限制模型前缀的形状：小写字母数字与 - _，长度 2..32。
// 前缀会出现在客户端的模型名里（<name>/<model>），也会进日志与文件名，所以必须可预期。
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,31}$`)

// Validate 校验一条配置。返回 nil 表示可用。
func (c ProviderConfig) Validate() error {
	if !nameRe.MatchString(c.Name) {
		return errors.New("名称只能用小写字母/数字/-/_，2–32 个字符，且以字母开头（它就是模型前缀）")
	}
	if builtinKinds[c.Name] {
		return fmt.Errorf("名称 %q 与内置渠道重名，请换一个", c.Name)
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return errors.New("base_url 必须以 http:// 或 https:// 开头")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("API Key 不能为空")
	}
	return nil
}

// ProviderStore 是 providers.json 的读写（每次调用都落盘，量小、以简单正确为先）。
type ProviderStore struct {
	mu    sync.RWMutex
	path  string
	items map[string]ProviderConfig
}

// NewProviderStore 打开（或初始化）providers.json。
func NewProviderStore(dir string) *ProviderStore {
	s := &ProviderStore{path: filepath.Join(dir, "providers.json"), items: map[string]ProviderConfig{}}
	s.load()
	return s
}

// List 返回全部来源，按名称排序（顺序稳定，面板不会每次刷新都换位置）。
func (s *ProviderStore) List() []ProviderConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProviderConfig, 0, len(s.items))
	for _, c := range s.items {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get 取一条配置。
func (s *ProviderStore) Get(name string) (ProviderConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.items[name]
	return c, ok
}

// Put 新增或覆盖一条配置（按 Name）。
func (s *ProviderStore) Put(cfg ProviderConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.Name = strings.ToLower(strings.TrimSpace(cfg.Name))
	switch strings.ToLower(strings.TrimSpace(cfg.ToolsMode)) {
	case "native", "shim", "none":
		cfg.ToolsMode = strings.ToLower(strings.TrimSpace(cfg.ToolsMode))
	default:
		// 留空或写错：按 SupportsTools 推，保持旧配置的行为不变。
		if cfg.SupportsTools {
			cfg.ToolsMode = "native"
		} else {
			cfg.ToolsMode = "none"
		}
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.DisplayName == "" {
		cfg.DisplayName = cfg.Name
	}
	if _, exists := s.Get(cfg.Name); !exists {
		cfg.AddedAt = time.Now().Format("2006-01-02 15:04")
	}
	s.mu.Lock()
	s.items[cfg.Name] = cfg
	s.mu.Unlock()
	return s.save()
}

// Remove 删除一条配置。
func (s *ProviderStore) Remove(name string) error {
	s.mu.Lock()
	delete(s.items, strings.ToLower(strings.TrimSpace(name)))
	s.mu.Unlock()
	return s.save()
}

func (s *ProviderStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []ProviderConfig
	if json.Unmarshal(raw, &list) != nil {
		return // 坏了就当空，不阻断启动（面板里能重新加回来）
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range list {
		if c.Name != "" {
			s.items[c.Name] = c
		}
	}
}

func (s *ProviderStore) save() error {
	s.mu.RLock()
	list := make([]ProviderConfig, 0, len(s.items))
	for _, c := range s.items {
		list = append(list, c)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	// 0600：里面是明文 API Key，等于密码。
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
