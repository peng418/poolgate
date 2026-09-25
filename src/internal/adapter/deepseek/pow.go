package deepseek

// pow.go DeepSeekHashV1 的求解：跑**官方那份 WASM**（用纯 Go 的 wazero，不需要 cgo）。
//
// 为什么不是自己实现哈希：上游用的是自研 DeepSeekHashV1，社区三个参考实现（Rust/Python）
// 全部调这份 WASM —— 没有可照抄的纯 Go 重实现。
// 好消息：这份 WASM **不注册任何 import**（无 wasm-bindgen 的 JS 依赖），所以能直接实例化。
//
// 实测导出（2026-09 本机用 wazero 打印）：
//
//	wasm_solve(i32,i32,i32,i32,i32,f64)      求解，结果写回 retptr
//	__wbindgen_add_to_stack_pointer(i32)->i32 拿临时栈指针
//	__wbindgen_export_0(i32,i32)->i32         分配器（签名就是 __wbindgen_malloc 的形状）
//
// 所以分配器要**按签名探测**而不是写死名字 —— 上游换构建时导出名可能变。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"poolgate/internal/channel"
	"poolgate/internal/errs"
)

// powChallenge 是一次 PoW 挑战（字段名与上游一致）。
type powChallenge struct {
	Algorithm   string  `json:"algorithm"`
	Challenge   string  `json:"challenge"`
	Salt        string  `json:"salt"`
	Signature   string  `json:"signature"`
	Difficulty  float64 `json:"difficulty"`
	ExpireAt    int64   `json:"expire_at"`
	ExpireAfter int64   `json:"expire_after"`
	TargetPath  string  `json:"target_path"`
}

// solver 持有已实例化的 WASM 与它导出的函数。
type solver struct {
	rt       wazero.Runtime
	mod      api.Module
	solve    api.Function
	alloc    api.Function
	addStack api.Function
	mu       sync.Mutex // wasm 实例不是并发安全的
}

// newSolver 实例化 PoW 求解器。
func newSolver(ctx context.Context, wasm []byte) (*solver, error) {
	rt := wazero.NewRuntime(ctx)
	mod, err := rt.Instantiate(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("实例化 PoW WASM 失败: %w", err)
	}
	solve := mod.ExportedFunction("wasm_solve")
	addStack := mod.ExportedFunction("__wbindgen_add_to_stack_pointer")
	if solve == nil || addStack == nil {
		return nil, fmt.Errorf("PoW WASM 缺导出函数（wasm_solve / __wbindgen_add_to_stack_pointer）")
	}
	alloc := pickAlloc(mod)
	if alloc == nil {
		return nil, fmt.Errorf("PoW WASM 找不到分配器（__wbindgen_malloc 或 __wbindgen_export_N）")
	}
	return &solver{rt: rt, mod: mod, solve: solve, alloc: alloc, addStack: addStack}, nil
}

// pickAlloc 按签名探测分配器：(i32,i32)->i32，优先名字里带 malloc 的那个。
func pickAlloc(mod api.Module) api.Function {
	if f := mod.ExportedFunction("__wbindgen_malloc"); f != nil {
		return f
	}
	for _, name := range []string{"__wbindgen_export_0", "__wbindgen_export_1", "__wbindgen_export_2"} {
		f := mod.ExportedFunction(name)
		if f == nil {
			continue
		}
		def := f.Definition()
		if len(def.ParamTypes()) == 2 && len(def.ResultTypes()) == 1 {
			return f
		}
	}
	return nil
}

func (s *solver) Close(ctx context.Context) { _ = s.rt.Close(ctx) }

// solve 解一个挑战，返回 answer（找不到解时 ok=false）。
func (s *solver) solve1(ctx context.Context, ch powChallenge) (answer int64, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := fmt.Sprintf("%s_%d_", ch.Salt, ch.ExpireAt)
	mem := s.mod.Memory()

	// retptr = add_to_stack(-16)：两个 i32(状态) + i64/f64(答案) 的临时区
	res, err := s.addStack.Call(ctx, uint64(uint32(0xFFFFFFF0))) // -16 的补码
	if err != nil {
		return 0, false, err
	}
	ret := uint32(res[0])
	defer func() { _, _ = s.addStack.Call(ctx, uint64(uint32(16))) }()

	write := func(b []byte) (uint32, error) {
		if len(b) == 0 {
			b = []byte{0}
		}
		r, err := s.alloc.Call(ctx, uint64(len(b)), 1)
		if err != nil {
			return 0, err
		}
		ptr := uint32(r[0])
		if !mem.Write(ptr, b) {
			return 0, fmt.Errorf("写 WASM 内存失败")
		}
		return ptr, nil
	}
	pc, err := write([]byte(ch.Challenge))
	if err != nil {
		return 0, false, err
	}
	pp, err := write([]byte(prefix))
	if err != nil {
		return 0, false, err
	}
	if _, err := s.solve.Call(ctx, uint64(ret), uint64(pc), uint64(len(ch.Challenge)),
		uint64(pp), uint64(len(prefix)), math.Float64bits(ch.Difficulty)); err != nil {
		return 0, false, err
	}
	raw, okMem := mem.Read(ret, 16)
	if !okMem {
		return 0, false, fmt.Errorf("读 WASM 结果失败")
	}
	status := int32(binary.LittleEndian.Uint32(raw[0:4]))
	val := math.Float64frombits(binary.LittleEndian.Uint64(raw[8:16]))
	if status != 0 {
		return 0, false, nil // 无解（挑战本身有问题）
	}
	return int64(val), true, nil
}

// powHeader 计算出 `X-Ds-Pow-Response` 的值：**base64(只含 6 个字段的 JSON)**。
// 注意不含 difficulty / expire_at —— 上游只要这 6 个。
func (s *solver) powHeader(ctx context.Context, ch powChallenge) (string, error) {
	answer, ok, err := s.solve1(ctx, ch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("PoW 无解（挑战参数可能已过期）")
	}
	payload := map[string]any{
		"algorithm":   ch.Algorithm,
		"challenge":   ch.Challenge,
		"salt":        ch.Salt,
		"answer":      answer,
		"signature":   ch.Signature,
		"target_path": ch.TargetPath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// loadWASM 下载并缓存 PoW WASM（失败要给可读原因：拿不到就没法对话，不能静默）。
type wasmCache struct {
	mu   sync.Mutex
	data []byte
}

func (c *wasmCache) get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) > 0 {
		return c.data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errs.New(errs.Transport, "下载 PoW WASM 失败（需要能访问 fe-static.deepseek.com）").
			WithChannel(string(channel.DeepSeek)).WithCause(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errs.New(errs.UpstreamFault, fmt.Sprintf("下载 PoW WASM 失败：HTTP %d", resp.StatusCode)).
			WithChannel(string(channel.DeepSeek))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, []byte("\x00asm")) {
		return nil, errs.New(errs.Parse, "PoW WASM 内容不对（不是 WebAssembly 模块）").
			WithChannel(string(channel.DeepSeek))
	}
	c.data = raw
	return raw, nil
}
