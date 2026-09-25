#!/usr/bin/env python3
"""模拟一个 OpenAI 兼容上游，用来在没有真实 API key 的情况下验证「API Key 式来源」。

用途：
  python3 tools/mock-openai-upstream.py 5099
然后在 PoolGate 里加一个来源，base_url 填 http://127.0.0.1:5099/v1、key 随便填。

它做三件事：
  GET  /v1/models                → 两个模型
  POST /v1/chat/completions      → 带 tools 的请求回**分片的** tool_calls（首片 id/name、
                                   后续片只有 arguments），不带 tools 的请求回文本。
                                   —— 这正是真实上游的两种形态，用来验证网关的归并逻辑。
  在 /tmp 下落一份 upstream_saw.json，记录它实际收到了什么（有没有 tools、消息角色、
  有没有 tool_call_id），便于人工核对「客户端要的东西有没有被透传上去」。

注意：这只是开发/验收工具，不参与打包（FPK 里没有它）。
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

OpenTag = "<tool_call>"
CloseTag = "</tool_call>"
SEEN_FILE = "/tmp/mock_upstream_saw.json"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # 静音默认访问日志
        pass

    def _json(self, obj, code=200):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.rstrip("/") == "/v1/models":
            if not (self.headers.get("Authorization") or "").startswith("Bearer "):
                return self._json({"error": {"message": "missing api key"}}, 401)
            return self._json({
                "object": "list",
                "data": [{"id": "mock-strong-1", "owned_by": "mock"},
                         {"id": "mock-strong-2", "owned_by": "mock"}],
            })
        self._json({"error": {"message": "not found"}}, 404)

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        if not (self.headers.get("Authorization") or "").startswith("Bearer "):
            return self._json({"error": {"message": "invalid api key"}}, 401)

        msgs = body.get("messages") or []
        with open(SEEN_FILE, "w") as f:
            json.dump({
                "model": body.get("model"),
                "stream": body.get("stream"),
                "has_tools": "tools" in body,
                "n_tools": len(body.get("tools") or []),
                "roles": [m.get("role") for m in msgs],
                "has_tool_call_id": any(m.get("tool_call_id") for m in msgs),
                "system_has_tool_prompt": OpenTag in " ".join((m.get("content") or "") for m in msgs if m.get("role") == "system"),
            }, f)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def w(s):
            self.wfile.write(s.encode())
            self.wfile.flush()

        # 「只会聊天的上游」模式：请求里没有 tools，但系统提示词里带着工具说明
        # （说明网关启用了工具调用模拟）→ 像聊天模型那样把工具调用**写成文本**。
        sys_text = " ".join((m.get("content") or "") for m in msgs if m.get("role") == "system")
        chat_only = ("tools" not in body) and (OpenTag in sys_text)
        if chat_only:
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"好的，我查一下。"}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"content":"'
              + OpenTag + '{\\"name\\":\\"get_weather\\",\\"arguments\\":{\\"city\\":\\"北京\\"}}' + CloseTag
              + '"}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"finish_reason":"stop"}]}\n\n')
        elif "tools" in body:
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":['
              '{"index":0,"id":"call_mock","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":['
              '{"index":0,"function":{"arguments":"{\\"city\\":"}}]}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":['
              '{"index":0,"function":{"arguments":"\\"北京\\"}"}}]}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls"}]}\n\n')
        else:
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"你好"}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"delta":{"content":"，我是模拟上游。"}}]}\n\n')
            w('data: {"id":"c1","choices":[{"index":0,"finish_reason":"stop"}],'
              '"usage":{"prompt_tokens":5,"completion_tokens":9}}\n\n')
        w("data: [DONE]\n\n")


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 5099
    print(f"mock OpenAI upstream on 127.0.0.1:{port}（收到的东西记在 {SEEN_FILE}）")
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
