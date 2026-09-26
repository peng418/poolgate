package gateway

import (
	"context"
	"net/http"
	"testing"

	"poolgate/internal/channel"
)

// tc 构造一个测试用 tool_call。
func tc(id, name, args string) channel.ToolCall {
	return channel.ToolCall{ID: id, Type: "function", Function: channel.FunctionCall{Name: name, Arguments: args}}
}

// TestCleanupOrphanToolCallsNoTraffic 无工具流量 → 零改动（changed=false，返回原 slice）。
func TestCleanupOrphanToolCallsNoTraffic(t *testing.T) {
	messages := []channel.Message{
		{Role: "system", Content: "hi"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("no-traffic should be unchanged")
	}
	if &out[0] != &messages[0] {
		t.Fatal("no-traffic should return the original slice")
	}
}

// TestCleanupOrphanToolCallWithoutResult 孤儿 tool_call（无结果）→ 清掉 ToolCalls。
func TestCleanupOrphanToolCallWithoutResult(t *testing.T) {
	messages := []channel.Message{
		{Role: "assistant", ToolCalls: []channel.ToolCall{
			tc("call_1", "Grep", "{}"),
		}},
		{Role: "user", Content: "continue"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan tool_call should be cleaned")
	}
	if out[0].ToolCalls != nil {
		t.Fatalf("tool_calls should be removed, got %#v", out[0])
	}
}

// TestCleanupOrphanPartialBatch 一批两个 tool_call，只有 c1 拿到结果 → 按 keepCalls
// 对称裁剪：调用侧只留 c1、无结果的 c2 被剔，两侧不残留半截配对。
func TestCleanupOrphanPartialBatch(t *testing.T) {
	messages := []channel.Message{
		{Role: "assistant", ToolCalls: []channel.ToolCall{
			tc("c1", "read", "{}"),
			tc("c2", "read", "{}"),
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "user", Content: "next"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("partial batch should be cleaned")
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "c1" {
		t.Fatalf("只应保留有结果的 c1，实际 %#v", out[0])
	}
	// 关键不变式：出站任何 tool 结果都必须有对应 tool_call，任何 tool_call 都必须有
	// 结果——否则上游判 11148（tool calls and tool results do not match）。
	assertPairingSymmetric(t, out)
}

// assertPairingSymmetric 断言出站消息两侧配对对称：每个 tool_call id 都有结果，
// 每个 tool 结果的 id 都有调用。task 要求「只留有结果配对的调用、孤儿结果整删，
// 两侧同口径」——该断言是两侧同口径的直接表达。
func assertPairingSymmetric(t *testing.T, msgs []channel.Message) {
	t.Helper()
	allCalls := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			if call.ID != "" {
				allCalls[call.ID] = true
			}
		}
	}
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		if m.Role != "tool" {
			continue
		}
		resultIDs[m.ToolCallID] = true
		if !allCalls[m.ToolCallID] {
			t.Fatalf("残留孤儿 tool 结果 %s——上游会判 11148：%#v", m.ToolCallID, msgs)
		}
	}
	for id := range allCalls {
		if !resultIDs[id] {
			t.Fatalf("残留无结果 tool_call %s——上游会判 11148：%#v", id, msgs)
		}
	}
}

// TestCleanupOrphanResultOnly 孤儿 tool 结果（无对应 tool_call）→ 整条消息删除。
func TestCleanupOrphanResultOnly(t *testing.T) {
	messages := []channel.Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "ghost", Content: "orphan"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan result should be cleaned")
	}
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("orphan tool message should be removed, got %#v", out)
	}
}

// TestCleanupOrphanPairingPreserved 正例零改动：完整配对的多 tool_call 轮 + 正常文本轮，
// 所有字段原样保留。
func TestCleanupOrphanPairingPreserved(t *testing.T) {
	grepArgs := `{"pattern":"foo","path":"."}`
	readArgs := `{"file_path":"a.go"}`
	messages := []channel.Message{
		{Role: "user", Content: "search"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{
			tc("call_1", "Grep", grepArgs),
			tc("call_2", "Read", readArgs),
		}},
		{Role: "tool", ToolCallID: "call_1", Content: "3 matches"},
		{Role: "tool", ToolCallID: "call_2", Content: "file body"},
		{Role: "user", Content: "keep going"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("fully paired round-trip must be zero-change")
	}
	if len(out) != 5 {
		t.Fatalf("message count changed: %d", len(out))
	}
	if len(out[1].ToolCalls) != 2 {
		t.Fatalf("tool_calls dropped from paired round-trip: %#v", out[1])
	}
	if out[1].ToolCalls[0].Function.Arguments != grepArgs {
		t.Fatalf("call_1 arguments mutated: %v", out[1].ToolCalls[0].Function.Arguments)
	}
}

// TestCleanupOrphanOutOfOrderToolBeforeResult 乱序：tool 结果消息出现在 assistant
// tool_call 之前（不按顺序但 id 齐全）→ 仍保留（按 id 集合配对，与顺序无关）。
func TestCleanupOrphanOutOfOrderToolBeforeResult(t *testing.T) {
	messages := []channel.Message{
		{Role: "tool", ToolCallID: "call_9", Content: "res"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("call_9", "f", "{}")}},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("id-complete out-of-order pairing must be preserved")
	}
	if len(out) != 2 {
		t.Fatalf("message count changed: %d", len(out))
	}
}

// TestCleanupOrphanDuplicateID 重复 tool_call id（两处引用同一结果 id）：
// 结果侧唯一、调用侧重复——每次按集合取并，保持「id 命中结果即保留」的最宽口径。
func TestCleanupOrphanDuplicateID(t *testing.T) {
	messages := []channel.Message{
		{Role: "tool", ToolCallID: "dup", Content: "r"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("dup", "a", "{}")}},
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("dup", "b", "{}")}},
	}
	_, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("duplicate id referencing an existing result must be preserved (widest keep)")
	}
}

// TestRepackToolResultBlocksInsertedNotice 复刻真实会话的卡死结构：Codex 的
// <image_resize_notice> 作为 developer 消息插在并行 tool 结果中间，上游判
// 11148 tool_call_sequence_broken。修复后结果必须连续、插入物后移、内容不变。
func TestRepackToolResultBlocksInsertedNotice(t *testing.T) {
	notice := "<image_resize_notice>resized</image_resize_notice>"
	messages := []channel.Message{
		{Role: "assistant", ToolCalls: []channel.ToolCall{
			tc("c00", "view_image", "{}"),
			tc("c01", "view_image", "{}"),
		}},
		{Role: "tool", ToolCallID: "c00", Content: "img0"},
		{Role: "developer", Content: notice},
		{Role: "tool", ToolCallID: "c01", Content: "img1"},
		{Role: "user", Content: "next"},
	}
	out, changed := repackToolResultBlocks(messages)
	if !changed {
		t.Fatal("插入物应触发重排")
	}
	if len(out) != 5 {
		t.Fatalf("消息数不应变化，实际 %d", len(out))
	}
	// 只调顺序不改内容：插入物的内容原样保留。
	if out[3].Content != notice {
		t.Fatalf("插入物内容被改动：%#v", out[3])
	}
	// 顺序：assistant 之后紧跟两条 tool 结果，developer 被移到其后。
	wantRoles := []string{"assistant", "tool", "tool", "developer", "user"}
	for i, w := range wantRoles {
		if out[i].Role != w {
			t.Fatalf("out[%d] 角色应为 %s，实际 %s（%#v）", i, w, out[i].Role, out)
		}
	}
	// 结果顺序保持 c00 -> c01。
	if out[1].ToolCallID != "c00" || out[2].ToolCallID != "c01" {
		t.Fatalf("结果顺序应为 c00 -> c01，实际 %s -> %s", out[1].ToolCallID, out[2].ToolCallID)
	}
}

// TestRepackToolResultBlocksNoInsert 无插入物（完整连续配对）→ 零改动（返回原 slice）。
func TestRepackToolResultBlocksNoInsert(t *testing.T) {
	messages := []channel.Message{
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("c1", "f", "{}")}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
		{Role: "user", Content: "n"},
	}
	out, changed := repackToolResultBlocks(messages)
	if changed {
		t.Fatal("完整配对不应改动")
	}
	if &out[0] != &messages[0] {
		t.Fatal("零改动应返回原 slice")
	}
}

// TestRepackToolResultBlocksNextGroupHeadNotSwallowed 回归（wild-work 真实会话 msg[181]
// 形态）：下一组 assistant.tool_calls 紧跟上一组结果时，绝不能被上一组的收集循环当
// 「插入物」吞掉——否则它自己那批结果永远得不到重排，上游照旧判 11148。
func TestRepackToolResultBlocksNextGroupHeadNotSwallowed(t *testing.T) {
	msgs := []channel.Message{
		{Role: "user", Content: "go"},
		// 第 1 组：exec_command ×2（结果连续，无插入物）。
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("c00", "exec_command", "{}"), tc("c01", "exec_command", "{}")}},
		{Role: "tool", ToolCallID: "c00", Content: "ok0"},
		{Role: "tool", ToolCallID: "c01", Content: "ok1"},
		// 第 2 组：view_image ×2，紧邻上一组结果，且自身结果被 notice 打断。
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("c10", "view_image", "{}"), tc("c11", "view_image", "{}")}},
		{Role: "tool", ToolCallID: "c10", Content: "img0"},
		{Role: "developer", Content: "<image_resize_notice>n"},
		{Role: "tool", ToolCallID: "c11", Content: "img1"},
		{Role: "developer", Content: "<image_resize_notice>n"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false，第二组未被重排")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	// 期望顺序：[user][a1][tool c00][tool c01][a2][tool c10][tool c11][dev][dev]
	want := []string{"", "", "c00", "c01", "", "c10", "c11", "", ""}
	for i, m := range out {
		if m.ToolCallID != want[i] {
			t.Fatalf("[%d] tool_call_id=%q，期望 %q；实际顺序 %v", i, m.ToolCallID, want[i], repackSeqOf(out))
		}
	}
	assertRepackPairsContiguous(t, out)
}

// TestRepackToolResultBlocksThreeConsecutiveGroups 回归：连续多组、仅末组含插入物
// ——确保组头识别在连续场景下不退化。
func TestRepackToolResultBlocksThreeConsecutiveGroups(t *testing.T) {
	msgs := []channel.Message{
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("a0", "x", "{}")}},
		{Role: "tool", ToolCallID: "a0", Content: "r"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("b0", "x", "{}"), tc("b1", "x", "{}")}},
		{Role: "tool", ToolCallID: "b0", Content: "r"},
		{Role: "tool", ToolCallID: "b1", Content: "r"},
		{Role: "assistant", ToolCalls: []channel.ToolCall{tc("c0", "x", "{}"), tc("c1", "x", "{}")}},
		{Role: "tool", ToolCallID: "c0", Content: "r"},
		{Role: "developer", Content: "<notice>"},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
		{Role: "developer", Content: "<notice>"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	assertRepackPairsContiguous(t, out)
}

func repackSeqOf(msgs []channel.Message) []string {
	var s []string
	for _, m := range msgs {
		if m.ToolCallID != "" {
			s = append(s, m.Role+":"+m.ToolCallID)
		} else {
			s = append(s, m.Role)
		}
	}
	return s
}

// assertRepackPairsContiguous 断言每个 assistant.tool_calls 的结果在其后连续出现。
func assertRepackPairsContiguous(t *testing.T, msgs []channel.Message) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		if len(msgs[i].ToolCalls) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, call := range msgs[i].ToolCalls {
			if call.ID != "" {
				want[call.ID] = true
			}
		}
		got := map[string]bool{}
		j := i + 1
		for j < len(msgs) {
			if msgs[j].Role != "tool" {
				break
			}
			if msgs[j].ToolCallID != "" {
				got[msgs[j].ToolCallID] = true
			}
			j++
		}
		if len(got) != len(want) {
			t.Fatalf("assistant[%d] 结果不连续：want=%d got=%d 序列=%v", i, len(want), len(got), repackSeqOf(msgs))
		}
	}
}

// ---------------------------------------------------------------------------
// 端到端：网关收到带孤儿 tool 消息的请求时，发给上游的 messages 里不含无法配对的条目
// ---------------------------------------------------------------------------

// captureChat 建一个把上游请求捕获到 got 的假渠道（工具原生支持）。
func captureChat(got *channel.ChatRequest, chunks ...channel.ChatCompletionChunk) *fakeChannel {
	return &fakeChannel{
		kind: channel.QoderCN,
		spec: channel.Spec{Kind: channel.QoderCN, Status: channel.Active, Tools: true},
		chat: func(ctx context.Context, c *channel.Credential, req channel.ChatRequest) (channel.Stream, error) {
			*got = req
			return &sliceStream{chunks: chunks}, nil
		},
	}
}

// TestGatewayStripsOrphanToolPairing 端到端：工具执行失败时客户端写了 assistant 的
// tool_calls 却没写回 c2 的结果——这条坏历史若原样重放，上游会对之后每条消息都 400。
// 网关必须在发出上游前把无结果的 c2 剔掉，且不残留孤儿 tool 结果。
func TestGatewayStripsOrphanToolPairing(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got, chunk("ok")), true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model": "qodercn/qwen3.8-max",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"file_path":"a.go"}`}},
				{"id": "c2", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"file_path":"b.go"}`}},
			}},
			{"role": "tool", "tool_call_id": "c1", "name": "read", "content": "a.go body"},
			// c2 没有结果：工具执行失败时客户端常常写不回结果。
			{"role": "user", "content": "next"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Messages) != 4 {
		t.Fatalf("应 4 条消息（c2 无结果不算孤儿消息），got %d：%#v", len(got.Messages), got.Messages)
	}
	asst := got.Messages[1]
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "c1" {
		t.Fatalf("上游拿到的 assistant 只应保留有结果的 c1：%#v", asst)
	}
	assertPairingSymmetric(t, got.Messages)
}

// TestGatewayStripsOrphanToolResult 端到端：只有 role:tool 结果、没有前置 tool_call
// 的历史（会话被裁剪/拼接损坏时会这样）→ 发给上游前整条删掉。
func TestGatewayStripsOrphanToolResult(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got, chunk("ok")), true)
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model": "qodercn/qwen3.8-max",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "tool", "tool_call_id": "ghost", "content": "orphan"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("孤儿 tool 消息应在发出上游前删除：%#v", got.Messages)
	}
}

// TestGatewayRepacksNoticeBetweenToolResults 端到端：Codex 的 <image_resize_notice>
// 作为 developer 消息插在并行 tool 结果中间——发出上游前必须把插入物挪到两条结果之后，
// 结果连续、消息数不变、内容不变。
func TestGatewayRepacksNoticeBetweenToolResults(t *testing.T) {
	var got channel.ChatRequest
	srv, _ := newTestGateway(t, captureChat(&got, chunk("ok")), true)
	notice := "<image_resize_notice>resized</image_resize_notice>"
	w := postJSON(t, srv, "/v1/chat/completions", map[string]any{
		"model": "qodercn/qwen3.8-max",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": nil, "tool_calls": []map[string]any{
				{"id": "c00", "type": "function", "function": map[string]any{"name": "view_image", "arguments": `{"path":"a.png"}`}},
				{"id": "c01", "type": "function", "function": map[string]any{"name": "view_image", "arguments": `{"path":"b.png"}`}},
			}},
			{"role": "tool", "tool_call_id": "c00", "name": "view_image", "content": "img0"},
			{"role": "developer", "content": notice},
			{"role": "tool", "tool_call_id": "c01", "name": "view_image", "content": "img1"},
			{"role": "user", "content": "next"},
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", w.Code, w.Body.String())
	}
	if len(got.Messages) != 6 {
		t.Fatalf("消息数不应变化，got %d：%#v", len(got.Messages), got.Messages)
	}
	// 期望角色序列：user assistant tool(c00) tool(c01) developer user
	wantRoles := []string{"user", "assistant", "tool", "tool", "developer", "user"}
	for i, r := range wantRoles {
		if got.Messages[i].Role != r {
			t.Fatalf("msg[%d] 角色应为 %s，实际 %s（%v）", i, r, got.Messages[i].Role, repackSeqOf(got.Messages))
		}
	}
	if got.Messages[2].ToolCallID != "c00" || got.Messages[3].ToolCallID != "c01" {
		t.Fatalf("结果顺序应保持 c00 -> c01：%#v", got.Messages)
	}
	if got.Messages[4].Content != notice {
		t.Fatalf("插入物内容不应改动：%#v", got.Messages[4])
	}
	assertRepackPairsContiguous(t, got.Messages)
}
