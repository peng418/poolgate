package traework

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// sseBody 造一段 SOLO 事件序列（TraeWork 上游实测的格式）。
func sseBody(events ...string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(strings.Join(events, "")))
}

func drain(t *testing.T, s channel.Stream) ([]channel.ChatCompletionChunk, error) {
	t.Helper()
	var out []channel.ChatCompletionChunk
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, chunk)
	}
}

// SOLO 事件序列 → 标准 chunk：增量文本、推理内容、usage、finish_reason 都要落到客户端。
func TestSoloStreamNormalizesEvents(t *testing.T) {
	s := newStream(sseBody(
		"event:metadata\ndata:{\"model\":\"glm-5.2\"}\n\n",
		"event:output\ndata:{\"response\":\"你好\"}\n\n",
		"event:output\ndata:{\"response\":\"，世界\",\"reasoning_content\":\"想一下\"}\n\n",
		"event:token_usage\ndata:{\"prompt_tokens\":3,\"completion_tokens\":5}\n\n",
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n",
	), "glm-5.2")
	defer s.Close()

	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("正常事件序列不应报错: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("期望 3 个 chunk（两段文本 + done），实际 %d 个: %+v", len(chunks), chunks)
	}
	if got := chunks[0].Choices[0].Delta.Content; got != "你好" {
		t.Fatalf("第 1 段内容应为「你好」，实际 %q", got)
	}
	if got := chunks[1].Choices[0].Delta.Content; got != "，世界" {
		t.Fatalf("第 2 段内容应为「，世界」，实际 %q", got)
	}
	if got := chunks[1].Choices[0].Delta.ReasoningContent; got != "想一下" {
		t.Fatalf("推理内容应透传，实际 %q", got)
	}
	// usage 挂在紧随其后的 chunk 上（这里是 done）。
	last := chunks[len(chunks)-1]
	if last.Usage == nil || last.Usage["completion_tokens"] == nil {
		t.Fatalf("usage 必须交给客户端，实际 %+v", last.Usage)
	}
	if last.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason 应为 stop，实际 %q", last.Choices[0].FinishReason)
	}
	for i, c := range chunks {
		if !strings.HasPrefix(c.ID, "chatcmpl-") {
			t.Fatalf("第 %d 个 chunk 的 id 应为 chatcmpl-*，实际 %q", i, c.ID)
		}
		if c.Model != "glm-5.2" {
			t.Fatalf("第 %d 个 chunk 的 model 应回填请求模型，实际 %q", i, c.Model)
		}
	}
}

// 红线一：上游 error 事件必须立刻变成客户端可见的错误 —— 不是攒到流结束（甚至永远不发）。
func TestSoloErrorEventSurfacesImmediately(t *testing.T) {
	// error 之后连接仍开着并继续吐事件（上游压着连接的真实形态）。
	s := newStream(sseBody(
		"event:output\ndata:{\"response\":\"开头\"}\n\n",
		"event:error\ndata:{\"code\":1005,\"message\":\"plan expired\"}\n\n",
		"event:output\ndata:{\"response\":\"不该出现\"}\n\n",
	), "glm-5.2")
	defer s.Close()

	chunks, err := drain(t, s)
	if err == nil {
		t.Fatal("error 事件之后的流必须以错误结束，不能静默关闭")
	}
	if !strings.Contains(err.Error(), "1005") {
		t.Fatalf("错误应带上游 code，实际 %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("error 事件后不应再产出 chunk，实际 %d 个", len(chunks))
	}
	if got := chunks[0].Choices[0].Delta.Content; got != "开头" {
		t.Fatalf("error 之前的内容必须已经交给客户端，实际 %q", got)
	}

	// 错误必须在 error 事件处就终止流，而不是被后面的 output 淹没。
	// drain 已经读到 EOF 说明流被正常关闭；这里显式再确认错误信息不丢。
	s2 := newStream(sseBody("event:error\ndata:{\"code\":1005,\"message\":\"plan expired\"}\n\n"), "glm-5.2")
	defer s2.Close()
	_, err = s2.Next()
	if err == nil || err == io.EOF {
		t.Fatal("只有 error 事件时，Next 必须返回该错误，不能当流正常结束")
	}
	if !strings.Contains(err.Error(), "1005") || !strings.Contains(err.Error(), "plan expired") {
		t.Fatalf("错误信息应带上游 code/message，实际 %v", err)
	}
}

// 红线二：什么都没收到 ≠ 成功。空流必须能判出来（网关据此回错误帧）。
func TestSoloEmptyStreamYieldsNothing(t *testing.T) {
	s := newStream(sseBody("\n\n", "event:metadata\ndata:{}\n\n"), "glm-5.2")
	defer s.Close()

	chunks, err := drain(t, s)
	if err != nil {
		t.Fatalf("空流不应产生 error（由调用方按「零 chunk」判失败）: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("空流不应产出 chunk，实际 %d 个", len(chunks))
	}
}

func TestParseSOLOLine(t *testing.T) {
	cases := []struct {
		name  string
		event string
		data  string
		check func(*testing.T, *soloEvent)
	}{
		{"output", "output", `{"response":"abc","reasoning_content":"xyz"}`, func(t *testing.T, e *soloEvent) {
			if e.Response != "abc" || e.Reasoning != "xyz" {
				t.Fatalf("output 解析错误: %+v", e)
			}
		}},
		{"done", "done", `{"finish_reason":"length"}`, func(t *testing.T, e *soloEvent) {
			if e.FinishReason != "length" {
				t.Fatalf("finish_reason 解析错误: %+v", e)
			}
		}},
		{"error", "error", `{"code":1005,"message":"boom"}`, func(t *testing.T, e *soloEvent) {
			if e.ErrorCode != 1005 || e.ErrorMessage != "boom" {
				t.Fatalf("error 解析错误: %+v", e)
			}
		}},
		{"malformed", "output", `{"response":`, func(t *testing.T, e *soloEvent) {
			if e.Response != "" {
				t.Fatalf("坏 JSON 不应产生内容: %+v", e)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, parseSOLOLine(c.event, c.data))
		})
	}
}

// 上游 HTTP 错误必须归一成有限枚举（D4），并带上游原话（可见性）。
func TestChatClassifiesUpstreamErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errs.Kind
	}{
		{401, `{"message":"unauthorized"}`, errs.SessionDead},
		{429, `{"message":"too many requests"}`, errs.SoftRate},
		{403, `{"code":1005,"message":"plan expired"}`, errs.HardCredit},
		{500, `{"message":"internal"}`, errs.UpstreamFault},
		{200, `{}`, errs.Parse}, // 200 但空体：走 Classify 时归为 Parse（不是成功）
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			}))
			defer srv.Close()

			a := NewWithBase(srv.URL, srv.URL, srv.Client())
			_, err := a.Chat(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"}, channel.ChatRequest{Model: "glm-5.2"})
			if c.status == 200 {
				if err != nil {
					t.Fatalf("200 空体不在这里报错（空流由网关判失败），实际 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("上游错误必须返回错误，不能静默")
			}
			k, ok := errs.KindOf(err)
			if !ok {
				t.Fatalf("必须是结构化错误，实际 %T", err)
			}
			if k != c.want {
				t.Fatalf("期望 %s，实际 %s（%v）", c.want, k, err)
			}
			ee, _ := err.(*errs.Error)
			if ee.Upstream == "" {
				t.Fatal("账户类错误必须带上游原话，便于定位（F1.2a）")
			}
			if ee.Account != "u1" {
				t.Fatalf("错误应标注账号 uid，实际 %q", ee.Account)
			}
		})
	}
}

// 请求体按 SOLO 协议构造：强制 stream、content 转多模态数组、空模型落到默认模型。
func TestBuildBodySoloShape(t *testing.T) {
	raw := buildBody(channel.ChatRequest{
		Model: "",
		Messages: []channel.Message{
			{Role: "developer", Content: "你是助手"},
			{Role: "user", Content: "你好"},
		},
	})
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatal("SOLO 通道必须强制 stream=true")
	}
	if obj["model"] != DefaultModel || obj["config_name"] != DefaultModel {
		t.Fatalf("空模型应落到默认模型 %s，实际 model=%v config_name=%v", DefaultModel, obj["model"], obj["config_name"])
	}
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer 角色应改写成 system，实际 %v", first["role"])
	}
	arr, ok := first["content"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("content 应为多模态数组，实际 %#v", first["content"])
	}
	if arr[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content 元素应为 text 类型，实际 %#v", arr[0])
	}
}

// 头族是 TraeWork 的鉴权契约（Cloud-IDE-JWT + 机器指纹），漏一个就 401。
func TestSoloHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	soloHeaders(req, &channel.Credential{
		UID:         "u1",
		AccessToken: "tok",
		Extra:       map[string]string{"machine_id": "m1", "device_id": "d1"},
	}, true)

	want := map[string]string{
		"Authorization":    "Cloud-IDE-JWT tok",
		"X-Cloudide-Token": "tok",
		"X-Ide-Token":      "tok",
		"X-Uid":            "u1",
		"X-App-Id":         AppID,
		"X-Machine-Id":     "m1",
		"X-Device-Id":      "d1",
		"Accept":           "text/event-stream",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Fatalf("头 %s 期望 %q，实际 %q", k, v, got)
		}
	}
}

// 模型目录：动态拉取、去重、剔除自定义模型；空目录判失败（不是「没有模型」的成功）。
func TestFetchModelsFiltersAndFailsOnEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"config_info_list":[
			{"config_name":"glm-5.2","display_config":{"display_name":"GLM-5.2"}},
			{"config_name":"glm-5.2","display_config":{"display_name":"重复"}},
			{"config_name":"custom_model_abc","display_config":{"display_name":"自定义"}},
			{"config_name":"my-custom","display_config":{"display_name":"自定义2","is_custom_model":true}},
			{"config_name":"","display_config":{"display_name":"空名"}},
			{"config_name":"kimi-k2.7","display_config":{"display_name":"Kimi"}}
		]}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	models, err := a.Models(context.Background(), &channel.Credential{UID: "u1", AccessToken: "tok"})
	if err != nil {
		t.Fatalf("目录拉取失败: %v", err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "glm-5.2,kimi-k2.7" {
		t.Fatalf("目录应去重并剔除自定义模型，实际 %v", ids)
	}

	// 空目录：必须报错，不能让网关以为「这个渠道没有模型」。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"config_info_list":[]}`)
	}))
	defer empty.Close()
	a2 := NewWithBase(empty.URL, empty.URL, empty.Client())
	if _, err := a2.Models(context.Background(), &channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("空目录必须判失败")
	} else if k, _ := errs.KindOf(err); k != errs.UpstreamFault {
		t.Fatalf("空目录应归一为 UpstreamFault，实际 %s", k)
	}
}

// 刷新被拒 → SessionDead（需重新授权），而不是笼统的 Transport/Parse。
func TestRefreshRejectedIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"message":"invalid refresh token"}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	_, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "rt"})
	if err == nil {
		t.Fatal("刷新被拒必须报错")
	}
	if k, _ := errs.KindOf(err); k != errs.SessionDead {
		t.Fatalf("期望 SessionDead，实际 %s", k)
	}

	// 没有 refresh token：不发请求，直接判需重新授权。
	if _, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1"}); err == nil {
		t.Fatal("缺 refresh token 必须报错")
	}
}

// 刷新的过期时间是毫秒时间戳，要换算成秒（否则会话会被当成 5 万年后过期）。
func TestRefreshNormalizesMillisExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Result":{"Token":"newtok","RefreshToken":"newrt","TokenExpireAt":1790241734000}}`)
	}))
	defer srv.Close()

	a := NewWithBase(srv.URL, srv.URL, srv.Client())
	nc, err := a.Refresh(context.Background(), &channel.Credential{UID: "u1", RefreshToken: "rt"})
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if nc.AccessToken != "newtok" || nc.RefreshToken != "newrt" {
		t.Fatalf("刷新后凭证未轮换: %+v", nc)
	}
	if y := nc.ExpiresAt.Year(); y != 2026 {
		t.Fatalf("毫秒时间戳未归一为秒，解析成 %s", nc.ExpiresAt)
	}
}
