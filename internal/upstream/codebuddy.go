// codebuddy.go 把既有 CodeBuddy 逻辑包装成 Provider 接口实现。
//
// 既有 Client 的 chat/refresh/models 方法已重命名为 chatStreamCodeBuddy /
// refreshTokenCodeBuddy / fetchModelsCodeBuddy（保留全部硬编码基址与请求头逻辑），
// 此处 codebuddyProvider 仅做委派，不重复实现。
package upstream

import (
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
)

// codebuddyProvider 包装既有 CodeBuddy 实现。
type codebuddyProvider struct {
	c *Client
}

func (p codebuddyProvider) Name() string { return codebuddyName }

func (p codebuddyProvider) ChatStream(c *Client, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return c.chatStreamCodeBuddy(a, body)
}

func (p codebuddyProvider) RefreshToken(c *Client, a *auth.Auth) error {
	return c.refreshTokenCodeBuddy(a)
}

func (p codebuddyProvider) FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error) {
	return c.fetchModelsCodeBuddy(a)
}

// StaticModels 返回 nil：CodeBuddy 的静态回退表由 handler 按区域（cn/intl）维护，
// 不走 Provider 接口（见 handler.staticModels / staticModelsIntl）。
func (p codebuddyProvider) StaticModels() []map[string]any { return nil }

// Classify 复用既有 CodeBuddy 错误分类。
func (p codebuddyProvider) Classify(status int, body string) ErrKind {
	return Classify(status, body)
}

// StreamTo CodeBuddy 上游已是 OpenAI 兼容 SSE，直接规范透传。
func (p codebuddyProvider) StreamTo(w http.ResponseWriter, rc io.Reader, model string) error {
	return Stream(w, rc)
}

// Aggregate CodeBuddy 上游已是 OpenAI 兼容 SSE，直接聚合。
func (p codebuddyProvider) Aggregate(rc io.Reader, model string) (map[string]any, error) {
	return Aggregate(rc)
}
