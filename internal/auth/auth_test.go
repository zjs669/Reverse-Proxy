package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
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
