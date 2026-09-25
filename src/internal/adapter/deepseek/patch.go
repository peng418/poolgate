package deepseek

// patch.go 把 DeepSeek 的 p/o/v patch 流解析成可用的增量。
//
// 与 OpenAI 的 delta 完全不同：这里每帧是「在某个路径上做某个操作」，
// **路径(p)与操作(o)跨帧持久化**（后续帧可以省略，沿用上一次的值），操作默认 SET。
// 文本通过 `response/fragments[-1]/content` 上的 APPEND 累积；fragments[].type 区分
// THINK（思考）与 RESPONSE（正文）。
//
// 这段逻辑照公开参考实现的 DeltaParser 重写（它对齐的是上游前端行为），
// 逐条分支都保留 —— 少一条分支的表现就是「回答丢字」，最难查。

import (
	"strings"

	"poolgate/internal/channel"
)

type fragment struct{ typ, content string }

type patchState struct {
	path    string
	hasPath bool
	op      string
	frags   []fragment
	status  string
	usage   int
}

// feed 消费一帧，返回（思考增量、正文增量、是否结束）。
func (s *patchState) feed(val map[string]any) (think, content string, finished bool) {
	if p, ok := val["p"].(string); ok {
		s.path, s.hasPath = p, true
	}
	if o, ok := val["o"].(string); ok {
		s.op = o
	}
	op := s.op
	if op == "" {
		op = "SET"
	}
	v, hasV := val["v"]
	if !hasV {
		return "", "", false
	}
	// 初始快照：还没有路径，且 v 里带 response
	if !s.hasPath {
		if m, ok := v.(map[string]any); ok {
			if resp, ok := m["response"].(map[string]any); ok {
				return s.initialSnapshot(resp)
			}
		}
	}
	if op == "BATCH" {
		if arr, ok := v.([]any); ok {
			return s.applyBatch(s.path, arr)
		}
	}
	return s.applyPath(s.path, op, v)
}

// initialSnapshot 处理首帧：整棵 response 树（含 fragments 全文与 status/usage）。
func (s *patchState) initialSnapshot(resp map[string]any) (string, string, bool) {
	if st, ok := resp["status"].(string); ok && st != "" {
		s.status = st
	}
	if n, ok := resp["accumulated_token_usage"].(float64); ok {
		s.usage = int(n)
	}
	var think, content strings.Builder
	if arr, ok := resp["fragments"].([]any); ok {
		s.frags = s.frags[:0]
		for _, it := range arr {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "" {
				continue
			}
			c, _ := m["content"].(string)
			s.frags = append(s.frags, fragment{typ: typ, content: c})
			if c == "" {
				continue
			}
			switch typ {
			case fragThink:
				think.WriteString(c)
			case fragResponse:
				content.WriteString(c)
			}
		}
	}
	return think.String(), content.String(), s.done()
}

// applyBatch 递归处理 BATCH：每个子项自带 p/o/v，路径是「父路径 + 子路径」。
func (s *patchState) applyBatch(parent string, arr []any) (string, string, bool) {
	var think, content strings.Builder
	var finished bool
	var subPath, subOp string
	for _, it := range arr {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		if p, ok := m["p"].(string); ok {
			subPath = p
		}
		if o, ok := m["o"].(string); ok {
			subOp = o
		}
		v, hasV := m["v"]
		if !hasV {
			continue
		}
		op := subOp
		if op == "" {
			op = "SET"
		}
		full := parent
		switch {
		case parent == "":
			full = subPath
		case subPath != "":
			full = parent + "/" + subPath
		}
		var t, c string
		var f bool
		if op == "BATCH" {
			t, c, f = s.applyBatch(full, asArray(v))
		} else {
			t, c, f = s.applyPath(full, op, v)
		}
		think.WriteString(t)
		content.WriteString(c)
		finished = finished || f
	}
	return think.String(), content.String(), finished
}

// applyPath 处理单条 patch。只认我们需要的几条路径，其余忽略（上游会带很多 UI 字段）。
func (s *patchState) applyPath(path, op string, val any) (string, string, bool) {
	switch strings.TrimPrefix(path, "/") {
	case "response/status":
		if st, ok := val.(string); ok {
			s.status = st
		}
		return "", "", s.done()
	case "response/accumulated_token_usage", "accumulated_token_usage":
		if n, ok := val.(float64); ok {
			s.usage = int(n)
		}
	case "response/fragments/-1/content":
		if t, ok := val.(string); ok && len(s.frags) > 0 {
			last := &s.frags[len(s.frags)-1]
			switch last.typ {
			case fragThink:
				last.content += t
				return t, "", false
			case fragResponse:
				last.content += t
				return "", t, false
			}
		}
	case "response/fragments":
		if op != "APPEND" {
			return "", "", false
		}
		var think, content strings.Builder
		for _, it := range asArray(val) {
			m, _ := it.(map[string]any)
			if m == nil {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "" {
				continue
			}
			c, _ := m["content"].(string)
			s.frags = append(s.frags, fragment{typ: typ, content: c})
			if c == "" {
				continue
			}
			switch typ {
			case fragThink:
				think.WriteString(c)
			case fragResponse:
				content.WriteString(c)
			}
		}
		return think.String(), content.String(), false
	}
	return "", "", false
}

// done 判断这一轮是否已结束。
func (s *patchState) done() bool {
	up := strings.ToUpper(s.status)
	return up == "FINISHED" || up == "INCOMPLETE"
}

func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// chunkOf 把一段增量包成标准 chunk。
func chunkOf(model, think, content string, finished bool) channel.ChatCompletionChunk {
	c := channel.ChatCompletionChunk{Model: model}
	ch := channel.ChunkChoice{}
	ch.Delta.Role = "assistant"
	ch.Delta.ReasoningContent = think
	ch.Delta.Content = content
	if finished {
		ch.FinishReason = "stop" // 工具调用时由网关/shim 纠正成 tool_calls
	}
	c.Choices = append(c.Choices, ch)
	return c
}
