// deepseek_pow.go DeepSeek 网页版工作量证明（PoW）求解。
//
// 协议（2026-11-02 抓包 + wasm 实测确认）：
//  1. POST /api/v0/chat/create_pow_challenge {"target_path": "/api/v0/chat/completion"}
//     返回 challenge{algorithm,salt,challenge,signature,difficulty,expire_at,expire_after,target_path}
//  2. 求解：找整数 answer 使 H(salt + "_" + expire_at + "_" + answer) 满足难度（前缀零位）
//     H 来自官方 sha3_wasm_bg.wasm 导出的 wasm_solve / wasm_deepseek_hash_v1。
//  3. 把 {algorithm,challenge,salt,answer,signature,target_path} 紧凑 JSON 后 base64，
//     作为 x-ds-pow-response 头发给 /api/v0/chat/completion。
//
// wasm ABI（wbindgen 约定，实测）：
//   __wbindgen_export_0(len, align) -> ptr          在 wasm 线性内存里分配
//   __wbindgen_add_to_stack_pointer(delta) -> sp    传 -16 拿到 16 字节返回值暂存区
//   wasm_solve(retptr, chalPtr, chalLen, prefixPtr, prefixLen, f64 difficulty)
//       写回 retptr+0 = i32 status（1=解出，0=未解出）、retptr+8 = f64 answer
//   wasm_deepseek_hash_v1(retptr, ptr, len)
//       写回 retptr+0 = ptr、retptr+4 = len（指向 hex 字符串）
//
// 关键约束：**PoW 头一次性**。同一个 x-ds-pow-response 第二次使用上游返回 40301
// INVALID_POW_RESPONSE（实测），因此每个 /chat/completion 请求都必须重新申请并求解。
package upstream

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

//go:embed sha3_wasm_bg.wasm
var deepseekPowWasm []byte

// deepseekPowRuntime 全局唯一的 wazero 运行时与编译产物。
//
// 为什么全局单例：CompileModule 是重活（几十毫秒级），而 Instantiate 很轻。
// 但 instantiate 不能完全省掉——wasm 的 __wbindgen_export_0 只分配不释放，
// 复用同一个实例会让线性内存随请求数线性增长，最终 OOM。
var deepseekPowRuntime struct {
	once sync.Once
	rt   wazero.Runtime
	mod  wazero.CompiledModule
	err  error
}

// deepseekPowInit 幂等初始化运行时（首次调用编译 wasm）。
func deepseekPowInit() error {
	deepseekPowRuntime.once.Do(func() {
		ctx := context.Background()
		rt := wazero.NewRuntime(ctx)
		mod, err := rt.CompileModule(ctx, deepseekPowWasm)
		if err != nil {
			_ = rt.Close(ctx)
			deepseekPowRuntime.err = fmt.Errorf("deepseek: compile pow wasm: %w", err)
			return
		}
		deepseekPowRuntime.rt = rt
		deepseekPowRuntime.mod = mod
	})
	return deepseekPowRuntime.err
}

// DeepSeekPowAnswer 求解结果。OK=false 表示 wasm 未解出（status=0）。
type DeepSeekPowAnswer struct {
	Answer int64
	OK     bool
}

// deepseekPowInstance 一个已实例化的 wasm 实例 + 它缓存的导出函数句柄。
//
// 为什么复用而不是每次 instantiate：InstantiateModule 会重建整个线性内存
// （wazero 默认 4MB 起），每个请求都做一次纯属浪费——PoW 求解本身只要 ~40ms，
// 实例化开销在同一量级。这里改成池化复用。
//
// 复用的前提是「wasm 的 bump allocator 只增不减」：
//   - 每次求解只分配 ~200 字节（challenge + prefix 两个字符串），
//   - 但 __wbindgen_export_0 没有对应的 free，指针会一直往上走。
// 因此给每个实例设分配水位上限（deepseekPowRecycleBytes），超过就丢弃重建，
// 避免线性内存无限增长。
type deepseekPowInstance struct {
	mod   api.Module
	alloc api.Function
	stack api.Function
	solve api.Function
	mem   api.Memory
	// used 本实例累计分配字节数（粗略：只算我们主动 put 的字符串）。
	used int
}

// deepseekPowRecycleBytes 单个实例的分配水位；超过即回收重建。
// 26KB 的模块、4MB 初始内存，1MB 水位足够跑几千次求解。
const deepseekPowRecycleBytes = 1 << 20

// deepseekPowPool 实例池（复用实例，避免每请求 instantiate）。
var deepseekPowPool struct {
	mu   sync.Mutex
	idle []*deepseekPowInstance
}

// deepseekAcquireInstance 取一个可用实例（池空则新建）。
func deepseekAcquireInstance() (*deepseekPowInstance, error) {
	if err := deepseekPowInit(); err != nil {
		return nil, err
	}
	deepseekPowPool.mu.Lock()
	if n := len(deepseekPowPool.idle); n > 0 {
		inst := deepseekPowPool.idle[n-1]
		deepseekPowPool.idle = deepseekPowPool.idle[:n-1]
		deepseekPowPool.mu.Unlock()
		return inst, nil
	}
	deepseekPowPool.mu.Unlock()
	return deepseekNewInstance()
}

// deepseekReleaseInstance 归还实例；超过分配水位则直接丢弃（Close 掉）。
func deepseekReleaseInstance(inst *deepseekPowInstance) {
	if inst == nil {
		return
	}
	ctx := context.Background()
	if inst.used > deepseekPowRecycleBytes {
		_ = inst.mod.Close(ctx)
		return
	}
	deepseekPowPool.mu.Lock()
	// 池子只留少量实例（并发上限很低，多了纯占内存）。
	if len(deepseekPowPool.idle) >= 8 {
		deepseekPowPool.mu.Unlock()
		_ = inst.mod.Close(ctx)
		return
	}
	deepseekPowPool.idle = append(deepseekPowPool.idle, inst)
	deepseekPowPool.mu.Unlock()
}

// deepseekNewInstance 实例化一个新 wasm 模块并缓存导出句柄。
func deepseekNewInstance() (*deepseekPowInstance, error) {
	ctx := context.Background()
	mod, err := deepseekPowRuntime.rt.InstantiateModule(ctx, deepseekPowRuntime.mod,
		wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("deepseek: instantiate pow wasm: %w", err)
	}
	inst := &deepseekPowInstance{
		mod:   mod,
		mem:   mod.Memory(),
		alloc: mod.ExportedFunction("__wbindgen_export_0"),
		stack: mod.ExportedFunction("__wbindgen_add_to_stack_pointer"),
		solve: mod.ExportedFunction("wasm_solve"),
	}
	if inst.mem == nil || inst.alloc == nil || inst.stack == nil || inst.solve == nil {
		_ = mod.Close(ctx)
		return nil, fmt.Errorf("deepseek: pow wasm 缺少必要导出（memory/__wbindgen_export_0/__wbindgen_add_to_stack_pointer/wasm_solve）")
	}
	return inst, nil
}

// SolveDeepSeekPow 用官方 wasm 求解一个 challenge。
// 返回 answer 与是否解出；wasm 加载失败或调用异常时返回 error。
func SolveDeepSeekPow(challenge, salt string, expireAt int64, difficulty float64) (DeepSeekPowAnswer, error) {
	inst, err := deepseekAcquireInstance()
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}
	defer deepseekReleaseInstance(inst)

	ctx := context.Background()
	put := func(s string) (uint32, uint32, error) {
		b := []byte(s)
		res, err := inst.alloc.Call(ctx, uint64(len(b)), 1)
		if err != nil {
			return 0, 0, fmt.Errorf("deepseek: pow alloc: %w", err)
		}
		ptr := uint32(res[0])
		if !inst.mem.Write(ptr, b) {
			return 0, 0, fmt.Errorf("deepseek: pow 写入 wasm 内存越界 (ptr=%d len=%d)", ptr, len(b))
		}
		inst.used += len(b)
		return ptr, uint32(len(b)), nil
	}

	// 返回值暂存区 16 字节（wbindgen 约定：i32 status + f64 answer）。
	sp, err := inst.stack.Call(ctx, deepseekNeg16())
	if err != nil {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: pow stack: %w", err)
	}
	retPtr := uint32(sp[0])
	defer func() { _, _ = inst.stack.Call(ctx, 16) }()

	chalPtr, chalLen, err := put(challenge)
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}
	// 前缀固定为 "<salt>_<expire_at>_"，answer 由 wasm 自己拼。
	prefixPtr, prefixLen, err := put(fmt.Sprintf("%s_%d_", salt, expireAt))
	if err != nil {
		return DeepSeekPowAnswer{}, err
	}

	if _, err := inst.solve.Call(ctx, uint64(retPtr), uint64(chalPtr), uint64(chalLen),
		uint64(prefixPtr), uint64(prefixLen), math.Float64bits(difficulty)); err != nil {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: wasm_solve: %w", err)
	}

	status, ok := inst.mem.ReadUint32Le(retPtr)
	if !ok {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: 读取 pow status 失败")
	}
	bits, ok := inst.mem.ReadUint64Le(retPtr + 8)
	if !ok {
		return DeepSeekPowAnswer{}, fmt.Errorf("deepseek: 读取 pow answer 失败")
	}
	if status == 0 {
		return DeepSeekPowAnswer{OK: false}, nil
	}
	return DeepSeekPowAnswer{Answer: int64(math.Float64frombits(bits)), OK: true}, nil
}

// DeepSeekPowChallenge 上游下发的 challenge（字段名与接口一致）。
type DeepSeekPowChallenge struct {
	Algorithm   string  `json:"algorithm"`
	Challenge   string  `json:"challenge"`
	Salt        string  `json:"salt"`
	Signature   string  `json:"signature"`
	Difficulty  float64 `json:"difficulty"`
	ExpireAt    int64   `json:"expire_at"`
	ExpireAfter int64   `json:"expire_after"`
	TargetPath  string  `json:"target_path"`
}

// BuildDeepSeekPowHeader 求解 challenge 并组装 x-ds-pow-response 头的值。
// JSON 必须紧凑（无空格）且字段顺序固定——上游按整串校验（实测 tampering 返回 40301）。
func BuildDeepSeekPowHeader(ch DeepSeekPowChallenge) (string, error) {
	ans, err := SolveDeepSeekPow(ch.Challenge, ch.Salt, ch.ExpireAt, ch.Difficulty)
	if err != nil {
		return "", err
	}
	if !ans.OK {
		return "", fmt.Errorf("deepseek: wasm_solve 未解出 challenge（difficulty=%v）", ch.Difficulty)
	}
	// 用 map 会打乱字段顺序，这里用固定顺序的结构体保证与浏览器一致。
	payload := struct {
		Algorithm  string `json:"algorithm"`
		Challenge  string `json:"challenge"`
		Salt       string `json:"salt"`
		Answer     int64  `json:"answer"`
		Signature  string `json:"signature"`
		TargetPath string `json:"target_path"`
	}{
		Algorithm:  ch.Algorithm,
		Challenge:  ch.Challenge,
		Salt:       ch.Salt,
		Answer:     ans.Answer,
		Signature:  ch.Signature,
		TargetPath: ch.TargetPath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("deepseek: marshal pow payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// deepseekNeg16 返回 -16 的 uint64 表示（wasm i32 参数按无符号传）。
// 不能直接写 uint64(uint32(int32(-16)))：常量表达式在编译期就溢出。
func deepseekNeg16() uint64 {
	v := int32(-16)
	return uint64(uint32(v))
}

// deepseekHashString 用 wasm 计算 H(s)，仅供测试断言 PoW 正确性。
func deepseekHashString(s string) (string, error) {
	inst, err := deepseekAcquireInstance()
	if err != nil {
		return "", err
	}
	defer deepseekReleaseInstance(inst)

	ctx := context.Background()
	hashFn := inst.mod.ExportedFunction("wasm_deepseek_hash_v1")
	if hashFn == nil {
		return "", fmt.Errorf("deepseek: pow wasm 缺少 hash 导出")
	}
	b := []byte(s)
	res, err := inst.alloc.Call(ctx, uint64(len(b)), 1)
	if err != nil {
		return "", err
	}
	ptr := uint32(res[0])
	if !inst.mem.Write(ptr, b) {
		return "", fmt.Errorf("deepseek: hash 写入 wasm 内存越界")
	}
	inst.used += len(b)

	sp, err := inst.stack.Call(ctx, deepseekNeg16())
	if err != nil {
		return "", err
	}
	retPtr := uint32(sp[0])
	defer func() { _, _ = inst.stack.Call(ctx, 16) }()
	if _, err := hashFn.Call(ctx, uint64(retPtr), uint64(ptr), uint64(len(b))); err != nil {
		return "", err
	}
	outPtr, ok := inst.mem.ReadUint32Le(retPtr)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash ptr 失败")
	}
	outLen, ok := inst.mem.ReadUint32Le(retPtr + 4)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash len 失败")
	}
	out, ok := inst.mem.Read(outPtr, outLen)
	if !ok {
		return "", fmt.Errorf("deepseek: 读取 hash 内容失败")
	}
	return string(out), nil
}

// 编译期断言：api.Memory 被上面的 mem 变量使用（避免 import 未使用）。
var _ api.Memory = nil
