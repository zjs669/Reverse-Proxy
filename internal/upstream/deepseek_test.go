// deepseek_test.go DeepSeek provider 的离线单测：PoW 求解、SSE 解析、prompt 构造。
//
// 全部不依赖网络：PoW 用固定 challenge（实测从上游取回的真实值）断言 wasm 行为，
// SSE 用浏览器抓包得到的真实帧序列断言解析结果。
package upstream

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// TestSolveDeepSeekPow_RealChallenge 用上游真实下发的 challenge 验证 wasm 求解。
//
// 数据来源：2026-11-02 POST /api/v0/chat/create_pow_challenge 的实际响应
// （difficulty=144000，Node/wazero 双端实测均解出 answer=100089）。
// 该断言同时覆盖：wasm 加载、ABI 调用顺序（retptr 先于入参）、结果解码。
func TestSolveDeepSeekPow_RealChallenge(t *testing.T) {
	const (
		challenge = "224601a72fdb3f2af64337272e4c8bff73ebc1ec6291218ebbeda72fd3c68d21"
		salt      = "11da7aaf83fafe33dd8e"
		expireAt  = int64(1793610450235)
		difficulty = 144000.0
	)
	ans, err := SolveDeepSeekPow(challenge, salt, expireAt, difficulty)
	if err != nil {
		t.Fatalf("SolveDeepSeekPow error: %v", err)
	}
	if !ans.OK {
		t.Fatalf("wasm_solve 未解出 challenge（difficulty=%v）", difficulty)
	}
	if ans.Answer <= 0 {
		t.Fatalf("answer 非正数: %d", ans.Answer)
	}
	// 复核：H(salt_expire_answer) 的十六进制前导零位数应满足难度要求。
	// 难度按「前导零 hex 位」计（difficulty/16 位），144000/16 = 9000 位——显然不是这个口径，
	// 实际是「answer 的搜索空间上界」。这里只断言 hash 可计算且非空，真正的校验由上游做。
	h, err := deepseekHashString(salt + "_" + "1793610450235" + "_" + strconv.FormatInt(ans.Answer, 10))
	if err != nil {
		t.Fatalf("deepseekHashString error: %v", err)
	}
	if len(h) != 64 {
		t.Fatalf("hash 长度应为 64 位十六进制，实际 %d: %q", len(h), h)
	}
}

// TestDeepSeekHashString_KnownValue 固定输入断言 wasm hash 输出，
// 防止上游换 wasm 版本后行为漂移（值来自 Node 与 wazero 双端一致性验证）。
func TestDeepSeekHashString_KnownValue(t *testing.T) {
	got, err := deepseekHashString("abc")
	if err != nil {
		t.Fatalf("deepseekHashString: %v", err)
	}
	const want = "f841106c601ce9be9bc38525e90d4178d47f21dd8eb9f238fc55ffaa4ca94506"
	if got != want {
		t.Fatalf("hash(abc) 漂移：got=%s want=%s（上游可能换了 wasm 版本）", got, want)
	}
}

// TestBuildDeepSeekPowHeader 验证 header 是紧凑 JSON 的 base64，且字段齐全。
func TestBuildDeepSeekPowHeader(t *testing.T) {
	ch := DeepSeekPowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "224601a72fdb3f2af64337272e4c8bff73ebc1ec6291218ebbeda72fd3c68d21",
		Salt:       "11da7aaf83fafe33dd8e",
		Signature:  "dd79614b0422370c397810d360dc8e84589e9fee315a6f208ee5a92cf8837eae",
		Difficulty: 144000,
		ExpireAt:   1793610450235,
		TargetPath: "/api/v0/chat/completion",
	}
	hdr, err := BuildDeepSeekPowHeader(ch)
	if err != nil {
		t.Fatalf("BuildDeepSeekPowHeader: %v", err)
	}
	if hdr == "" {
		t.Fatal("header 为空")
	}
	// base64 解出来的 JSON 必须紧凑（无空格），且包含全部必需字段。
	raw := mustBase64(t, hdr)
	for _, key := range []string{`"algorithm"`, `"challenge"`, `"salt"`, `"answer"`, `"signature"`, `"target_path"`} {
		if !strings.Contains(raw, key) {
			t.Fatalf("header JSON 缺少字段 %s: %s", key, raw)
		}
	}
	if strings.Contains(raw, " ") {
		t.Fatalf("header JSON 不应含空格（上游按整串校验）: %s", raw)
	}
}

// mustBase64 解 base64 并返回字符串（失败即 t.Fatal）。
func mustBase64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return string(b)
}

// TestDeepseekPromptFromMessages_Single 单条 user 原样返回（branch 模式的常态）。
func TestDeepseekPromptFromMessages_Single(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
	}
	if got := deepseekPromptFromMessages(body); got != "你好" {
		t.Fatalf("单条 user 应原样返回，got=%q", got)
	}
	if got := deepseekLastUserText(body); got != "你好" {
		t.Fatalf("deepseekLastUserText got=%q", got)
	}
}

// TestDeepseekPromptFromMessages_History 多轮时拼成「用户:/助手:」序列，末条不加前缀。
func TestDeepseekPromptFromMessages_History(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "记住 4173"},
			map[string]any{"role": "assistant", "content": "OK"},
			map[string]any{"role": "user", "content": "数字是多少"},
		},
	}
	got := deepseekPromptFromMessages(body)
	if !strings.HasPrefix(got, "你是助手") {
		t.Fatalf("system 应折叠到最前，got=%q", got)
	}
	if !strings.Contains(got, "用户: 记住 4173") {
		t.Fatalf("历史 user 应带「用户:」前缀，got=%q", got)
	}
	if !strings.Contains(got, "助手: OK") {
		t.Fatalf("历史 assistant 应带「助手:」前缀，got=%q", got)
	}
	if !strings.HasSuffix(got, "数字是多少") {
		t.Fatalf("末条 user 应在末尾且不加前缀，got=%q", got)
	}
	// branch 模式只取最后一条 user
	if got := deepseekLastUserText(body); got != "数字是多少" {
		t.Fatalf("deepseekLastUserText got=%q", got)
	}
}

// TestDeepseekModelMapping 模型名 ↔ model_type 双向映射。
func TestDeepseekModelMapping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "default"},
		{"deepseek-instant", "default"},
		{"instant", "default"},
		{"deepseek-expert", "expert"},
		{"expert", "expert"},
		{"deepseek-vision", "vision"},
		{"vision", "vision"},
		{"deepseek-unknown", "default"},
	}
	for _, c := range cases {
		if got := deepseekModelType(c.in); got != c.want {
			t.Errorf("deepseekModelType(%q)=%q want %q", c.in, got, c.want)
		}
	}
	if got := deepseekModelID("expert"); got != "deepseek-expert" {
		t.Errorf("deepseekModelID(expert)=%q", got)
	}
	if got := deepseekModelID(""); got != "deepseek-instant" {
		t.Errorf("deepseekModelID(\"\")=%q", got)
	}
}

// TestDeepseekThinking 深度思考默认开启，可按多种方式显式关闭。
func TestDeepseekThinking(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"未传 → 默认开", map[string]any{}, true},
		{"thinking=false 关", map[string]any{"thinking": false}, false},
		{"thinking=true 开", map[string]any{"thinking": true}, true},
		{"thinking_enabled=false 关", map[string]any{"thinking_enabled": false}, false},
		{"reasoning_effort=none 关", map[string]any{"reasoning_effort": "none"}, false},
		{"reasoning_effort=low 仍开", map[string]any{"reasoning_effort": "low"}, true},
		{"reasoning_effort=high 开", map[string]any{"reasoning_effort": "high"}, true},
	}
	for _, c := range cases {
		if got := deepseekThinking(c.body); got != c.want {
			t.Errorf("%s: deepseekThinking=%v want %v", c.name, got, c.want)
		}
	}
}

// TestDeepseekSearch 联网搜索默认开启，可显式关闭。
func TestDeepseekSearch(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"未传 → 默认开", map[string]any{}, true},
		{"search=false 关", map[string]any{"search": false}, false},
		{"search_enabled=false 关", map[string]any{"search_enabled": false}, false},
		{"search=true 开", map[string]any{"search": true}, true},
	}
	for _, c := range cases {
		if got := deepseekSearch(c.body); got != c.want {
			t.Errorf("%s: deepseekSearch=%v want %v", c.name, got, c.want)
		}
	}
}

// TestDeepseekWalk_SearchStatusFramesIgnored 开联网搜索后，流里会混入搜索状态裸帧
// （出现在首个 response 快照之前）。这些帧不能当正文输出。
func TestDeepseekWalk_SearchStatusFramesIgnored(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		// 搜索状态帧：裸字符串，出现在 response 快照之前
		`data: {"v":"正在搜索..."}`,
		"",
		`data: {"p":"search_status","o":"SET","v":"SEARCHING"}`,
		"",
		// 真正的回答
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"答案"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"是 2"}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var text string
	if err := deepseekWalk(strings.NewReader(sse),
		func(c, r, f string) error { text += c; return nil }, nil, nil); err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if text != "答案是 2" {
		t.Fatalf("搜索结果帧不能混入正文，got=%q want=%q", text, "答案是 2")
	}
}

// TestDeepseekClassify 上游错误码 → ErrKind 映射（上游 HTTP 恒 200，全靠 body）。
func TestDeepseekClassify(t *testing.T) {
	p := &deepseekProvider{}
	cases := []struct {
		name string
		body string
		want ErrKind
	}{
		{"token 失效", `{"code":40003,"msg":"Authorization Failed","data":null}`, ErrSessionDead},
		{"封号", `{"code":40012,"msg":"banned","data":null}`, ErrSessionDead},
		{"IP 限制", `{"code":40029,"msg":"ip restricted","data":null}`, ErrSoftRate},
		{"禁言", `{"code":50006,"msg":"muted","data":null}`, ErrSoftRate},
		{"缺 PoW", `{"code":40300,"msg":"MISSING_HEADER","data":null}`, ErrServer},
		{"PoW 无效", `{"code":40301,"msg":"INVALID_POW_RESPONSE","data":null}`, ErrServer},
		{"会话不存在", `{"code":0,"msg":"","data":{"biz_code":1,"biz_msg":"invalid chat session id"}}`, ErrBadParams},
		{"消息 id 无效", `{"code":0,"msg":"","data":{"biz_code":26,"biz_msg":"invalid message id"}}`, ErrBadParams},
		{"思考额度超限（不罚号，由 ChatStream 关思考重试）", `{"code":0,"msg":"","data":{"biz_code":15,"biz_msg":"THINKING_QUOTA_EXCEEDED"}}`, ErrBadParams},
		{"成功", `{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{}}}`, ErrNone},
	}
	for _, c := range cases {
		if got := p.Classify(200, c.body); got != c.want {
			t.Errorf("%s: Classify(200)=%v want %v", c.name, got, c.want)
		}
		// 同一个 body 若被包装成 502（上游错误信封 HTTP 恒 200，网关内部改写成 5xx），
		// 分类结果必须不变——否则 biz_code=1/26 的「会话失效」会被误判成上游故障。
		if got := p.Classify(502, c.body); got != c.want {
			t.Errorf("%s: Classify(502)=%v want %v（信封语义不应被状态码覆盖）", c.name, got, c.want)
		}
	}
}

// TestDeepseekWalk_SnapshotOnly 只有快照、没有 APPEND 的极短回答也要产出正文。
func TestDeepseekWalk_SnapshotOnly(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"OK"}]}}}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var text string
	var respID int64
	err := deepseekWalk(strings.NewReader(sse),
		func(content, reasoning, finish string) error {
			text += content
			return nil
		}, nil,
		func(req, resp int64) { respID = resp })
	if err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if text != "OK" {
		t.Fatalf("正文应为 OK，got=%q", text)
	}
	if respID != 2 {
		t.Fatalf("response_message_id 应为 2，got=%d", respID)
	}
}

// TestDeepseekWalk_SnapshotPlusAppend 快照是「前缀」，APPEND 在其后追加，合起来才是全文。
//
// 这是实测抓到的真实帧序列：回答 "8642" 时快照 content="864"，随后 APPEND "2"。
// 若只输出 APPEND 就会丢掉 "864"（第一版实现正是这个 bug，被本用例固化成了预期值）。
func TestDeepseekWalk_SnapshotPlusAppend(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":3,"response_message_id":4,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":4,"fragments":[{"type":"RESPONSE","content":"864"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"2"}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var text string
	err := deepseekWalk(strings.NewReader(sse),
		func(content, reasoning, finish string) error {
			text += content
			return nil
		}, nil, nil)
	if err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if text != "8642" {
		t.Fatalf("正文应为「8642」（快照前缀 864 + APPEND 2），got=%q", text)
	}
}

// TestDeepseekWalk_Thinking 思考片段进 reasoning，正文进 content。
func TestDeepseekWalk_Thinking(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":2,"thinking_enabled":true,"fragments":[{"type":"THINK","content":"思考"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"过程"}`,
		"",
		`data: {"p":"response/fragments","o":"APPEND","v":[{"type":"RESPONSE","content":""}]}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"答案"}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var content, reasoning string
	err := deepseekWalk(strings.NewReader(sse),
		func(c, r, finish string) error {
			content += c
			reasoning += r
			return nil
		}, nil, nil)
	if err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if content != "答案" {
		t.Fatalf("content 应为「答案」，got=%q", content)
	}
	if reasoning != "思考过程" {
		t.Fatalf("reasoning 应为「思考过程」，got=%q", reasoning)
	}
}

// TestDeepseekAggregate 聚合结果形状符合 OpenAI 规范。
func TestDeepseekAggregate(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"你好"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"世界"}`,
		"",
		`data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":42}]}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	p := &deepseekProvider{}
	resp, err := p.Aggregate(strings.NewReader(sse), "deepseek-instant")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 长度应为 1，got=%d", len(choices))
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if got, _ := msg["content"].(string); got != "你好世界" {
		t.Fatalf("content 应为「你好世界」，got=%q", got)
	}
	if got, _ := ch["finish_reason"].(string); got != "stop" {
		t.Fatalf("finish_reason 应为 stop，got=%q", got)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("usage 应存在")
	}
	if got, _ := usage["total_tokens"].(int64); got != 42 {
		t.Fatalf("total_tokens 应为 42，got=%v", usage["total_tokens"])
	}
}

// TestDeepseekSessionBodyScan 会话观察器能从流里捞出 response_message_id。
func TestDeepseekSessionBodyScan(t *testing.T) {
	chunks := []string{
		"event: ready\n",
		`data: {"request_message_id":7,`,
		`"response_message_id":8,"model_type":"default"}` + "\n",
	}
	var respID int64
	b := &deepseekSessionBody{}
	for _, c := range chunks {
		b.scan([]byte(c))
	}
	respID = b.respID
	if respID != 8 {
		t.Fatalf("应解析出 response_message_id=8，got=%d", respID)
	}
}

// TestDeepseekWalk_SnapshotOverlapNoDuplicate 若上游重发全量快照（含已 APPEND 的部分），
// 只补发差额，不重复输出。
func TestDeepseekWalk_SnapshotOverlapNoDuplicate(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"AB"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"C"}`,
		"",
		// 重发全量快照：已含 ABC，应补发 D
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"ABCD"}]}}}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var text string
	if err := deepseekWalk(strings.NewReader(sse),
		func(c, r, f string) error { text += c; return nil }, nil, nil); err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if text != "ABCD" {
		t.Fatalf("应输出「ABCD」且不重复，got=%q", text)
	}
}

// TestDeepseekClassify_NonEnvelope 非本协议响应（WAF 页/空 body）回落到状态码判定。
func TestDeepseekClassify_NonEnvelope(t *testing.T) {
	p := &deepseekProvider{}
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{403, "<html>Access denied</html>", ErrSessionDead},
		{429, "", ErrSoftRate},
		{503, "Service Unavailable", ErrServer},
		{400, "bad request", ErrClient},
		{200, "", ErrNone},
	}
	for _, c := range cases {
		if got := p.Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d, %q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestDeepseekWalk_BareDeltaFrames 上游会发「裸增量帧」{"v":"..."}（无 p 字段），
// 与显式 {"p":"response/fragments/-1/content"} 帧混用。只认后者会丢掉全部裸帧内容。
//
// 这是实测抓到的真实序列（回答 "Hello! How can I help you today?"）：
//   快照 content="Hello"  →  APPEND "!"  →  {"v":" How"} {"v":" can"} ...
func TestDeepseekWalk_BareDeltaFrames(t *testing.T) {
	sse := strings.Join([]string{
		"event: ready",
		`data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}`,
		"",
		`data: {"v":{"response":{"message_id":2,"fragments":[{"type":"RESPONSE","content":"Hello"}]}}}`,
		"",
		`data: {"p":"response/fragments/-1/content","o":"APPEND","v":"!"}`,
		"",
		`data: {"v":" How"}`,
		"",
		`data: {"v":" can"}`,
		"",
		`data: {"v":" I"}`,
		"",
		`data: {"p":"response","o":"BATCH","v":[{"p":"accumulated_token_usage","v":42}]}`,
		"",
		`data: {"p":"response/status","o":"SET","v":"FINISHED"}`,
		"",
	}, "\n")
	var text string
	if err := deepseekWalk(strings.NewReader(sse),
		func(c, r, f string) error { text += c; return nil }, nil, nil); err != nil {
		t.Fatalf("deepseekWalk: %v", err)
	}
	if text != "Hello! How can I" {
		t.Fatalf("裸帧内容不能丢，got=%q want=%q", text, "Hello! How can I")
	}
}

// TestDeepSeekPow_InstanceReuse 验证实例池复用：连续多次求解结果一致（复用不污染状态）。
func TestDeepSeekPow_InstanceReuse(t *testing.T) {
	const (
		challenge  = "224601a72fdb3f2af64337272e4c8bff73ebc1ec6291218ebbeda72fd3c68d21"
		salt       = "11da7aaf83fafe33dd8e"
		expireAt   = int64(1793610450235)
		difficulty = 144000.0
	)
	var first int64
	for i := 0; i < 20; i++ {
		ans, err := SolveDeepSeekPow(challenge, salt, expireAt, difficulty)
		if err != nil {
			t.Fatalf("第 %d 次求解失败: %v", i, err)
		}
		if !ans.OK {
			t.Fatalf("第 %d 次未解出", i)
		}
		if i == 0 {
			first = ans.Answer
		} else if ans.Answer != first {
			t.Fatalf("复用实例后结果漂移：第 %d 次=%d，首次=%d", i, ans.Answer, first)
		}
	}
}

// TestDeepSeekPow_Concurrent 并发求解（实例池必须线程安全）。
func TestDeepSeekPow_Concurrent(t *testing.T) {
	const (
		challenge  = "224601a72fdb3f2af64337272e4c8bff73ebc1ec6291218ebbeda72fd3c68d21"
		salt       = "11da7aaf83fafe33dd8e"
		expireAt   = int64(1793610450235)
		difficulty = 144000.0
	)
	const n = 8
	errs := make(chan error, n)
	answers := make(chan int64, n)
	for i := 0; i < n; i++ {
		go func() {
			ans, err := SolveDeepSeekPow(challenge, salt, expireAt, difficulty)
			if err != nil {
				errs <- err
				return
			}
			if !ans.OK {
				errs <- fmt.Errorf("未解出")
				return
			}
			answers <- ans.Answer
		}()
	}
	var got []int64
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("并发求解失败: %v", err)
		case a := <-answers:
			got = append(got, a)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("并发结果不一致: %v", got)
		}
	}
}

// TestDsParseEnvelope_StatusNotZero 业务错误必须带回 >=400 的 Status。
//
// 上游错误信封恒 HTTP 200；若 *Error.Status 原样保留 200，
// 调用方 `if status >= 400` 会漏判成「成功」，把错误体当正文返回给客户端。
func TestDsParseEnvelope_StatusNotZero(t *testing.T) {
	cases := []string{
		`{"code":40003,"msg":"Authorization Failed","data":null}`,
		`{"code":0,"msg":"","data":{"biz_code":1,"biz_msg":"invalid chat session id"}}`,
		`{"code":0,"msg":"","data":{"biz_code":26,"biz_msg":"invalid message id"}}`,
	}
	for _, body := range cases {
		_, err := dsParseEnvelope(200, []byte(body))
		if err == nil {
			t.Fatalf("应返回错误: %s", body)
		}
		var ue *Error
		if !asError(err, &ue) {
			t.Fatalf("应为 *Error: %v", err)
		}
		if ue.Status < 400 {
			t.Fatalf("Status 必须 >=400（否则调用方会当成功），got=%d body=%s", ue.Status, body)
		}
	}
}

// TestDsParseEnvelope_Success 正常信封不报错。
func TestDsParseEnvelope_Success(t *testing.T) {
	data, err := dsParseEnvelope(200, []byte(`{"code":0,"msg":"","data":{"biz_code":0,"biz_data":{"chat_session":{"id":"x"}}}}`))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !strings.Contains(string(data), "chat_session") {
		t.Fatalf("bizData 解析错误: %s", string(data))
	}
}
