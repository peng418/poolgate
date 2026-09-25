// providers.go 装配层：把「API Key 式来源」挂进注册表与账号池，并做连通性测试。
//
// 为什么放在 boot：只有这一层同时认识 适配器 / 注册表 / 账号池 / 配置存储。
// 控制台不直接构造适配器（它只调一个接口），装配细节留在装配层 —— 加一种新来源时
// 只用改这一个文件，控制台与网关都不用动。
package boot

import (
	"context"
	"strings"
	"time"

	"poolgate/internal/adapter/openaiup"
	"poolgate/internal/channel"
	"poolgate/internal/errs"
	"poolgate/internal/pool"
	"poolgate/internal/registry"
	"poolgate/internal/store"
)

// ProviderAdapter 由一个来源配置构造适配器。
func ProviderAdapter(cfg store.ProviderConfig) *openaiup.Adapter {
	return openaiup.New(openaiup.Config{
		Name: cfg.Name, DisplayName: cfg.DisplayName, BaseURL: cfg.BaseURL,
		APIKey: cfg.APIKey, Models: cfg.Models, SupportsTools: cfg.SupportsTools, Notes: cfg.Notes,
	})
}

// MountProvider 把一个来源挂进注册表与账号池（新增或热更新都走它）。
//
// 重复挂载是安全的：registry.Register 覆盖同 Kind，pool.AddFor 覆盖同 UID ——
// 改配置（换 key / 换 base_url / 改能力位）后立刻生效，不需要重启进程。
func MountProvider(cfg store.ProviderConfig, p *pool.Pool) {
	a := ProviderAdapter(cfg)
	registry.Register(a, a.Spec())
	if p != nil {
		p.AddFor(a.Kind(), a.Credential())
	}
}

// UnmountProvider 摘除一个来源（删除接入源时调用）。
func UnmountProvider(name string, p *pool.Pool) {
	kind := channel.Kind(strings.ToLower(strings.TrimSpace(name)))
	registry.Remove(kind)
	if p != nil {
		p.Remove(kind, string(kind)) // 合成账号的 UID 就是来源名
	}
}

// ProbeResult 是「连通性测试」的结果（面板向导第 3 步）。
//
// 每一项都独立报告：鉴权通了但没模型、有模型但工具调用拿不到 —— 这些是完全不同的
// 结论，混成一个「成功/失败」会让用户拿不到可行动的下一步。
type ProbeResult struct {
	AuthOK     bool     `json:"auth_ok"`
	AuthNote   string   `json:"auth_note"`
	ModelCount int      `json:"model_count"`
	Models     []string `json:"models"`
	// ModelsSource: upstream（上游 /models）| local（手填清单）| none
	ModelsSource string `json:"models_source"`
	ToolsOK      bool   `json:"tools_ok"`
	ToolsNote    string `json:"tools_note"`
	ToolsSample  string `json:"tools_sample,omitempty"`
	TTFTMs       int64  `json:"ttft_ms"`
	Error        string `json:"error,omitempty"`
}

// ProbeProvider 对一个**尚未保存**的配置做连通性测试。
//
// 这是「添加接入源」里最值钱的一步：鉴权、目录、**工具调用**、首字延迟四项分开报。
// 工具调用那项直接决定这个来源能不能给 Studio / Claude Code 当后端 —— 0.4.2 之前
// 我们把 tools 静默丢掉，客户端拿到的是一堆乱码，就是缺了这一步验证。
func ProbeProvider(ctx context.Context, cfg store.ProviderConfig) ProbeResult {
	var res ProbeResult
	a := ProviderAdapter(cfg)
	cred := a.Credential()

	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// 1) 鉴权 + 模型目录
	models, err := a.Models(ctx, &cred)
	if err != nil {
		res.Error = err.Error()
		res.AuthNote = "拿不到模型目录"
		if k, ok := errs.KindOf(err); ok {
			res.AuthNote = "拿不到模型目录（" + string(k) + "）"
		}
		return res
	}
	res.AuthOK = true
	res.AuthNote = "key 有效"
	res.ModelCount = len(models)
	for i, m := range models {
		if i >= 8 {
			break
		}
		res.Models = append(res.Models, m.ID)
	}
	if len(models) > 0 {
		switch models[0].Source {
		case channel.SourceUpstream:
			res.ModelsSource = "upstream"
		case channel.SourceLocal:
			res.ModelsSource = "local"
		default:
			res.ModelsSource = "none"
		}
	}

	// 2) 实打一次请求：既能测首字延迟，也能验证工具调用
	pick := models[0].ID
	creq := channel.ChatRequest{
		Model:     pick,
		Messages:  []channel.Message{{Role: "user", Content: "用 get_weather 工具查一下北京天气，必须调用工具。"}},
		MaxTokens: 256,
	}
	if cfg.SupportsTools {
		creq.Tools = []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name": "get_weather", "description": "查询指定城市的天气",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []string{"city"},
				},
			},
		}}
	}
	st, err := a.Chat(ctx, &cred, creq)
	if err != nil {
		res.Error = err.Error()
		if !res.AuthOK {
			res.AuthNote = "请求被拒绝"
		}
		return res
	}
	defer st.Close()

	start := time.Now()
	first := true
	gotContent := false
	for {
		c, err := st.Next()
		if err != nil {
			break
		}
		for _, ch := range c.Choices {
			if first && (ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" || len(ch.Delta.ToolCalls) > 0) {
				res.TTFTMs = time.Since(start).Milliseconds()
				first = false
			}
			if ch.Delta.Content != "" || ch.Delta.ReasoningContent != "" {
				gotContent = true
			}
			for _, tc := range ch.Delta.ToolCalls {
				if strings.TrimSpace(tc.Function.Name) != "" {
					res.ToolsOK = true
					res.ToolsSample = strings.TrimSpace(tc.Function.Name + " " + tc.Function.Arguments)
				}
			}
		}
		if res.ToolsOK {
			break // 拿到结构化工具调用就可以停了，不用把整段跑完
		}
	}

	switch {
	case !cfg.SupportsTools:
		res.ToolsNote = "该来源声明为「不支持工具调用」：带 tools 的请求会被明确拒绝（这是有意的，不是丢弃）"
	case res.ToolsOK:
		res.ToolsNote = "上游返回了结构化 tool_calls —— 可以给 Studio / Claude Code 当后端"
	case gotContent:
		res.ToolsNote = "上游有回复，但没给出结构化 tool_calls：它可能不支持工具调用，或这次恰好没用。建议实测一轮再决定是否声明支持"
	default:
		res.ToolsNote = "没有收到任何内容"
	}
	return res
}

// ProviderPresets 返回主流平台预设（面板「添加接入源」直接列出来）。
func ProviderPresets() []openaiup.Preset { return openaiup.Presets() }
