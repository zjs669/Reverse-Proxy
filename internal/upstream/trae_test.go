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

// GetUserToken 的鉴权头必须带登录会话 X-Cloudide-Session。
// 实机验证（2026-11-04）：该接口认两种凭证任一——未过期的 X-Cloudide-Token，或长期有效的
// X-Cloudide-Session（= data.source_id）；只发前者时，JWT 一过期就 401/20101，账号只能人工重登。
func TestTraeRefreshHeadersCarrySession(t *testing.T) {
	const sid = "test-source-id.0123456789abcdef"
	expired := makeTestJWT(t, map[string]any{
		"data": map[string]any{"id": "1234567890123456", "source_id": sid},
		"exp":  1793701120,
	})
	p := &traeProvider{}

	t.Run("过期 JWT 仍与会话一起带上", func(t *testing.T) {
		a := &auth.Auth{Provider: "trae", AccessToken: expired, UID: "1234567890123456"}
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		p.applyRefreshHeaders(req, a, p.resolveSourceID(a))
		if got := req.Header.Get("X-Cloudide-Session"); got != sid {
			t.Fatalf("X-Cloudide-Session = %q, want %q", got, sid)
		}
		if got := req.Header.Get("X-Cloudide-Token"); got != expired {
			t.Fatalf("X-Cloudide-Token 应与 accessToken 同源，got %q", got)
		}
	})

	// 令牌完全丢了也不能拦住换新：靠 extra.sourceId 照样能换，宁可不发令牌头。
	t.Run("无 accessToken 时靠会话换新", func(t *testing.T) {
		a := &auth.Auth{Provider: "trae", UID: "1", Extra: map[string]string{"sourceId": sid}}
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
		if err != nil {
			t.Fatal(err)
		}
		p.applyRefreshHeaders(req, a, p.resolveSourceID(a))
		if _, ok := req.Header["X-Cloudide-Token"]; ok {
			t.Fatalf("accessToken 为空时不应设置 X-Cloudide-Token")
		}
		if got := req.Header.Get("X-Cloudide-Session"); got != sid {
			t.Fatalf("X-Cloudide-Session = %q, want %q", got, sid)
		}
	})
}
