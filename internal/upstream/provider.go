// provider.go 定义上游供应商抽象层。
//
// 背景：网关早期只支持 CodeBuddy（硬编码在 Client 各方法里）。为支持 Trae / Qoder
// 等多供应商，引入 Provider 接口——每个供应商实现自己的「基址 / 请求头 / 请求体变形 /
// SSE 翻译 / 模型列表 / 凭证刷新 / 错误分类」。Client 持有一个 providers 注册表，
// 按 auth.Auth.Provider 字段分派。未识别的 provider 回落 codebuddy（向后兼容）。
package upstream

import (
	"io"
	"net/http"

	"reverse-proxy/internal/auth"
)

// codebuddyName 默认 provider 名（回落目标）。
const codebuddyName = "codebuddy"

// Provider 抽象单一上游供应商协议。
//
// 所有方法均接收 *Client（共享 HTTP 客户端 / 超时 / 指纹开关等基建），以及该次请求
// 选中的账号凭证 a。ChatStream 返回原始上行流 rc（调用方负责 Close）；非 2xx 时
// rc 为 nil、respBody 为上游响应体（供调用方 Classify），err 仅表示传输层失败。
type Provider interface {
	// Name 返回 provider 标识（"codebuddy" / "trae" / "qoder"）。
	Name() string
	// ChatStream 发起一次对话并返回原始上游流。
	ChatStream(c *Client, a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	// RefreshToken 刷新凭证（原地修改 a 的 AccessToken/RefreshToken/ExpiresAt）。
	RefreshToken(c *Client, a *auth.Auth) error
	// FetchModels 拉取动态模型列表（失败由调用方回退静态表）。
	FetchModels(c *Client, a *auth.Auth) ([]ModelInfo, error)
	// StaticModels 动态列表失败时的回退静态模型表（可能为空，表示沿用网关默认表）。
	StaticModels() []map[string]any
	// Classify 按 HTTP 状态码 + body 对本供应商的错误分类。
	Classify(status int, body string) ErrKind
	// StreamTo 把上游「原生」SSE 转写为标准 OpenAI SSE 并写入 w（流式响应）。
	// CodeBuddy 上游已输出 OpenAI 兼容 SSE，直接透传规范化；Trae/Qoder 需先做协议翻译。
	StreamTo(w http.ResponseWriter, rc io.Reader, model string) error
	// Aggregate 把上游「原生」SSE 聚合成单个 OpenAI chat.completion 响应对象。
	Aggregate(rc io.Reader, model string) (map[string]any, error)
}

// providerFor 按账号 provider 取对应实现；空/未知回落 codebuddy。
func (c *Client) providerFor(a *auth.Auth) Provider {
	if a == nil {
		return c.providers[codebuddyName]
	}
	if p, ok := c.providers[a.ProviderOrDefault()]; ok {
		return p
	}
	return c.providers[codebuddyName]
}

// ChatStream 按 provider 分派到具体上游。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return c.providerFor(a).ChatStream(c, a, body)
}

// RefreshToken 按 provider 分派到具体上游。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return c.providerFor(a).RefreshToken(c, a)
}

// FetchModels 按 provider 分派到具体上游。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	return c.providerFor(a).FetchModels(c, a)
}

// Usage 拉取账号额度（可选能力：只有实现了 Usage 的 provider 支持，
// 当前为 qoder / codebuddy / trae；DeepSeek 是网页会话，无额度接口）。
// 不支持的 provider 返回 (nil, nil)，由调用方按“无额度信息”处理。
func (c *Client) Usage(a *auth.Auth) (any, error) {
	if up, ok := c.providerFor(a).(interface {
		Usage(*Client, *auth.Auth) (any, error)
	}); ok {
		return up.Usage(c, a)
	}
	return nil, nil
}

// ClassifyFor 按 provider 分派错误分类。
func (c *Client) ClassifyFor(a *auth.Auth, status int, body string) ErrKind {
	return c.providerFor(a).Classify(status, body)
}

// ProviderByName 返回指定 provider 实现（不存在返回 nil）。
func (c *Client) ProviderByName(name string) Provider {
	return c.providers[name]
}

// ProviderFor 返回账号对应的 provider 实现（供 handler 调用 StreamTo/Aggregate）。
func (c *Client) ProviderFor(a *auth.Auth) Provider {
	return c.providerFor(a)
}

// ProviderStaticModels 返回指定 provider 的静态回退模型表（不存在返回 nil）。
func (c *Client) ProviderStaticModels(name string) []map[string]any {
	if p, ok := c.providers[name]; ok {
		return p.StaticModels()
	}
	return nil
}

// registerProviders 在 New() 内注册全部已知 provider。
// trae/qoder 用指针注册：它们持有跨调用共享的 modelMap（FetchModels 写、resolveModel 读），
// 若存值则每次 providerFor 取出的都是副本，modelMap 永远为空（映射失效）。
func (c *Client) registerProviders() {
	c.providers = map[string]Provider{
		codebuddyName: codebuddyProvider{c: c},
		"trae":        &traeProvider{c: c},
		"qoder":       &qoderProvider{c: c},
		// qoder_intl 与 qoder 共用同一份实现：差异只在基址（center.qoder.sh /
		// api3.qoder.sh），而基址由账号自身的 domain/region 决定（见 qoderRegion）。
		// 单独注册是为了让「国际号」在 auth.json、池、管理台里一眼可辨，
		// 而不是混在 qoder_* 里靠翻 domain 才知道打的是哪家。
		"qoder_intl": &qoderProvider{c: c},
		"deepseek":    &deepseekProvider{c: c},
	}
}
