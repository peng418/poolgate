package deepseek

// pow.go DeepSeekHashV1 的求解：跑**官方那份 WASM**（用纯 Go 的 wazero，不需要 cgo）。
//
// 为什么不是自己实现哈希：上游用的是自研 DeepSeekHashV1，社区三个参考实现（Rust/Python）
// 全部调这份 WASM —— 没有可照抄的纯 Go 重实现。
// 好消息：这份 WASM **不注册任何 import**（无 wasm-bindgen 的 JS 依赖），所以能直接实例化。
//
// 实测导出（2026-09 本机用 wazero 打印）：
//
//	wasm_solve(i32,i32,i32,i32,i32,f64)      求解，结果写回 retptr；**status!=0 才算解出**
//	__wbindgen_add_to_stack_pointer(i32)->i32 拿临时栈指针
//	__wbindgen_export_N(i32,i32)->i32         分配器（签名就是 __wbindgen_malloc 的形状）
//
// 所以分配器与求解函数都要**按名字/签名探测**而不是写死编号 —— 上游换构建时导出名可能变
// （参考实现 pow.rs:82-134 就是这么探测的）。

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
	"sort"
	"strings"
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
	// wasm_solve：优先按显式名字取，取不到再按唯一签名 (i32,i32,i32,i32,i32,f64)->() 探测
	// —— 参考实现 pow.rs:99-134 就是这么做的（上游换 WASM 构建时导出名可能变）。
	solve := mod.ExportedFunction("wasm_solve")
	if solve == nil {
		solve = pickSolve(mod)
	}
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
//
// 不能用固定名列表（`__wbindgen_export_0/1/2`）：参考实现 pow.rs:82-97 是按
// **前缀 `__wbindgen_export_` + 签名**探测的 —— 上游换 WASM 构建时导出编号会变，
// 写死编号会在新版本上直接起不来（这正是本文件开头注释所强调的）。
func pickAlloc(mod api.Module) api.Function {
	if f := mod.ExportedFunction("__wbindgen_malloc"); f != nil {
		return f
	}
	var names []string
	for name, def := range mod.ExportedFunctionDefinitions() {
		if !strings.HasPrefix(name, "__wbindgen_export_") {
			continue
		}
		if isAllocSig(def) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names) // 多个候选时取编号最小者，保证结果稳定
	return mod.ExportedFunction(names[0])
}

func isAllocSig(def api.FunctionDefinition) bool {
	p, r := def.ParamTypes(), def.ResultTypes()
	return len(p) == 2 && p[0] == api.ValueTypeI32 && p[1] == api.ValueTypeI32 &&
		len(r) == 1 && r[0] == api.ValueTypeI32
}

// pickSolve 按唯一签名 (i32,i32,i32,i32,i32,f64)->() 探测求解函数（参考实现 pow.rs:113-133）。
func pickSolve(mod api.Module) api.Function {
	var names []string
	for name, def := range mod.ExportedFunctionDefinitions() {
		p, r := def.ParamTypes(), def.ResultTypes()
		if len(p) != 6 || len(r) != 0 {
			continue
		}
		if p[0] != api.ValueTypeI32 || p[1] != api.ValueTypeI32 || p[2] != api.ValueTypeI32 ||
			p[3] != api.ValueTypeI32 || p[4] != api.ValueTypeI32 || p[5] != api.ValueTypeF64 {
			continue
		}
		names = append(names, name)
	}
	// 只在签名唯一时认领（多个同签名函数时无法判断哪个是求解器）。
	if len(names) != 1 {
		return nil
	}
	return mod.ExportedFunction(names[0])
}

func (s *solver) Close(ctx context.Context) { _ = s.rt.Close(ctx) }

// solve 解一个挑战，返回 answer（找不到解时 ok=false）。
func (s *solver) solve1(ctx context.Context, ch powChallenge) (answer int64, ok bool, err error) {
	// 只认 DeepSeekHashV1：参考实现 pow.rs:146-148 同样在求解前拒绝其它 algorithm，
	// 免得把别的算法塞进这份 WASM 算出垃圾。
	if ch.Algorithm != "DeepSeekHashV1" {
		return 0, false, fmt.Errorf("不支持的 PoW 算法：%s", ch.Algorithm)
	}
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
	// **status == 0 才是无解**，非 0 表示求解成功 —— 参考实现 pow.rs:210-212
	// （`if status == 0 { return Err(PowError::NoSolution) }`）与 gpt4free pow.py:80-84
	// 两处独立实现一致。这里之前写反了（把成功当无解、把无解当成功），
	// 表现就是「真上游上 PoW 永远解不出来 / 拿 0 当答案」。
	if status == 0 {
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

// guestPowHeader 计算出 `X-DS-Guest-PoW-Response` 的值：**base64(只含 salt/answer 的 JSON)**。
//
// 与 powHeader 唯一的区别就是载荷形状 —— 上游对「还没登录时的登录类接口」只收这两个
// 字段（算法/挑战/签名它按 salt 自己反查）。但**难度一样是真难度**：实测
// login_by_mobile_sms 的 difficulty=80000（发码接口只有 20），所以还是得老实跑 WASM，
// 随便塞一个 answer 会被回 40301 INVALID_POW_RESPONSE。
//
// 编码与官方前端一致：bundle 里这一层是 `encoder: btoa` → 标准 base64（带 padding）。
func (s *solver) guestPowHeader(ctx context.Context, ch powChallenge) (string, error) {
	answer, ok, err := s.solve1(ctx, ch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("PoW 无解（挑战参数可能已过期）")
	}
	raw, err := json.Marshal(map[string]any{"salt": ch.Salt, "answer": answer})
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
