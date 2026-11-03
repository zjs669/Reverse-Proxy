package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"reverse-proxy/internal/auth"
)

// makeTestJWT 拼一个不签名的三段式 JWT（网关只读载荷，不验签）。
func makeTestJWT(t *testing.T, payload map[string]any) string {
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

// X-Cloudide-Session 必须与当前 accessToken 同源：
// extra 为空时从 JWT 的 data.source_id 自动解析，显式配置时以显式值为准。
func TestTraeSessionHeaderFromJWT(t *testing.T) {
	const sid = "test-source-id.0123456789abcdef"
	tok := makeTestJWT(t, map[string]any{
		"data": map[string]any{"id": "1234567890123456", "source_id": sid},
		"exp":  1793701120,
	})

	p := &traeProvider{}
	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{"从 JWT 自动解析", nil, sid},
		{"显式 extra 覆盖", map[string]string{"sourceId": "explicit-sid"}, "explicit-sid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &auth.Auth{Provider: "trae", AccessToken: tok, UID: "1234567890123456", Extra: c.extra}
			req, err := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			p.applyChatHeaders(req, a, "")
			if got := req.Header.Get("X-Cloudide-Session"); got != c.want {
				t.Fatalf("X-Cloudide-Session = %q, want %q", got, c.want)
			}
			if got := req.Header.Get("X-Cloudide-Token"); got != tok {
				t.Fatalf("X-Cloudide-Token 应与 accessToken 同源，got %q", got)
			}
		})
	}
}

// 载荷里没有 source_id 时不应发送该头（而不是发空值）。
func TestTraeSessionHeaderAbsent(t *testing.T) {
	tok := makeTestJWT(t, map[string]any{"data": map[string]any{"id": "1"}})
	a := &auth.Auth{Provider: "trae", AccessToken: tok, UID: "1"}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/", nil)
	(&traeProvider{}).applyChatHeaders(req, a, "")
	if _, ok := req.Header["X-Cloudide-Session"]; ok {
		t.Fatalf("无 source_id 时不应设置 X-Cloudide-Session")
	}
}
