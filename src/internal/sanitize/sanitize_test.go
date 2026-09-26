package sanitize

// 出站脱敏回归测试。用例从 wild-work internal/upstream/sanitize_test.go 平移，
// 只保留有明确上游实测依据的那些（身份句两个变体、Main branch、billing header、
// cc_* 键值、Codex CLI 模板句、github.com/anthropics 反馈句、裸 11128），
// 另补一条「body 不可解析时原样返回、绝不丢消息」的降级护栏。
//
// 依赖 wild-work 私有 API（PrepareBody / 带 ChatMeta 的 ChatStream）的集成用例
// 不在此移植——本包是纯函数，渠道侧另有各自的 buildBody 回归测试。

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestIdentityRewritten(t *testing.T) {
	out := cleanText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := cleanText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := cleanText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := cleanText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11-128 以打断精确匹配。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := cleanText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Errorf("error code not rewritten as expected: %q", out)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := cleanText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// cc_* 键值整段删除：billing header 的尾随键值对同样命中黑名单。
func TestTrailingKVStripped(t *testing.T) {
	out := cleanText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := cleanText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 正常对话里的 Anthropic 链接（非反馈整句）不该被改写：预检命中进入净化，
// 但改写层只动精确匹配的整句。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := cleanText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// 降级护栏：body 不可解析时（哪怕含指纹、已进入净化流程）也必须原样返回，
// 绝不丢消息、绝不阻塞请求。
func TestMessagesUnparseableBodyReturnedAsIs(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"`) // 截断的非法 JSON
	out := Messages(in)
	if string(out) != string(in) {
		t.Fatalf("不可解析 body 应原样返回，实际被改为 %q", out)
	}
}

// body 级正向：合法 body 里 system 的指纹被改写，消息条数与其余内容不变。
func TestMessagesRewritesAndKeepsShape(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := Messages(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("净化后 body 不是合法 JSON: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息条数应不变（2 条），实际 %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, ccIdentity) || strings.Contains(sys, "x-anthropic-billing-header") {
		t.Errorf("指纹残留: %q", sys)
	}
	if !strings.Contains(sys, "official CLI tool for Claude") {
		t.Errorf("身份句未改写: %q", sys)
	}
	if got, _ := msgs[1].(map[string]any)["content"].(string); got != "hi" {
		t.Errorf("无关消息被改动: %q", got)
	}
}
