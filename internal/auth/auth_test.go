package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func jwtOf(t *testing.T, payload map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "RS256", "typ": "JWT"}) + "." + enc(payload) + ".sig"
}

// Trae 的 X-Uid / X-Cloudide-Session 都来自 JWT 载荷，必须能稳定取出。
func TestJWTDataString(t *testing.T) {
	tok := jwtOf(t, map[string]any{
		"data": map[string]any{
			"id":        "1234567890123456",
			"source_id": "test-source-id.0123456789abcdef",
			"tenant_id": "test-tenant-id",
		},
		"exp": 1793701120,
	})
	cases := map[string]string{
		"id":        "1234567890123456",
		"source_id": "test-source-id.0123456789abcdef",
		"tenant_id": "test-tenant-id",
		"missing":   "",
	}
	for key, want := range cases {
		if got := JWTDataString(tok, key); got != want {
			t.Errorf("JWTDataString(%q) = %q, want %q", key, got, want)
		}
	}
	if got := JWTSourceID(tok); got != "test-source-id.0123456789abcdef" {
		t.Errorf("JWTSourceID = %q, want %q", got, "test-source-id.0123456789abcdef")
	}
}

// 非法输入不能 panic，一律返回空串（向导据此决定是否报错）。
func TestJWTDataStringInvalid(t *testing.T) {
	for _, tok := range []string{"", "not-a-jwt", "a.b", "a.!!!.c"} {
		if got := JWTDataString(tok, "id"); got != "" {
			t.Errorf("JWTDataString(%q) = %q, want empty", tok, got)
		}
		if got := JWTSourceID(tok); got != "" {
			t.Errorf("JWTSourceID(%q) = %q, want empty", tok, got)
		}
	}
}

// JWTExp 必须对**任意长度**的载荷都解出 exp。
// 回归场景：旧版 cmd/login 自己写解码，先补 '=' 再用 RawURLEncoding（不接受填充），
// 载荷 base64url 长度不是 4 的倍数时必然 illegal base64 data → exp=0 →
// 登录写入 farFuture → 账号 8h 后静默死亡。这里扫完 mod 4 的全部余数。
func TestJWTExpAllPaddingCases(t *testing.T) {
	const want int64 = 1793703426
	for n := 0; n < 12; n++ {
		tok := jwtOf(t, map[string]any{
			"data": map[string]any{"id": "4501873774052416"},
			"note": strings.Repeat("x", n), // 调长度，覆盖 len%4 = 0..3
			"exp":  want,
		})
		seg := strings.Split(tok, ".")[1]
		if got := JWTExp(tok); got != want {
			t.Errorf("payload_len%%4=%d (len=%d): JWTExp = %d, want %d", len(seg)%4, len(seg), got, want)
		}
	}
	for _, tok := range []string{"", "a.b", "a.!!!.c", jwtOf(t, map[string]any{"iat": 1})} {
		if got := JWTExp(tok); got != 0 {
			t.Errorf("JWTExp(%q) = %d, want 0", tok, got)
		}
	}
}

// trae 凭证的 expiresAt 以令牌自身 exp 为权威：farFuture 占位值必须被收紧，
// 否则 NeedsRefresh 永假、token 看护永不续期也不告警（8h 短寿命凭证的死循环）。
func TestParseTraeExpiresAtFollowsJWTExp(t *testing.T) {
	exp := time.Now().Add(8 * time.Hour).Unix()
	tok := jwtOf(t, map[string]any{"data": map[string]any{"id": "u1"}, "exp": exp})
	const farFuture = int64(4102444800) // 2100-01-01

	mk := func(t *testing.T, stored string) *Auth {
		t.Helper()
		raw := []byte(`{"provider":"trae","uid":"u1","accessToken":` + jsonStr(t, tok) +
			`,"expiresAt":` + stored + `}`)
		a, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		return a
	}
	if a := mk(t, "4102444800"); a.ExpiresAt != exp {
		t.Errorf("farFuture 占位值应被收紧为 JWT exp: got %d want %d", a.ExpiresAt, exp)
	}
	if a := mk(t, "0"); a.ExpiresAt != exp {
		t.Errorf("未写 expiresAt 应取 JWT exp: got %d want %d", a.ExpiresAt, exp)
	}
	if a := mk(t, "100"); a.ExpiresAt != 100 {
		t.Errorf("存储值更早时不应放大: got %d want 100", a.ExpiresAt)
	}
	// 非 trae 供应商不改写（OAuth 等家的 accessToken 不能一律当 Cloud-IDE-JWT）。
	cb, err := Parse([]byte(`{"provider":"codebuddy","uid":"u1","accessToken":` + jsonStr(t, tok) + `,"expiresAt":` + "4102444800" + `}`))
	if err != nil {
		t.Fatalf("Parse codebuddy: %v", err)
	}
	if cb.ExpiresAt != farFuture {
		t.Errorf("codebuddy 的 expiresAt 不应被改: got %d", cb.ExpiresAt)
	}
}

func jsonStr(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
