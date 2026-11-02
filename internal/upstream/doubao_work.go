// doubao_work.go 豆包工作（Doubao Work / 豆包桌面客户端）供应商实现。
//
// 背景：豆包工作与「豆包网页版」(doubao.go) 共用同一套后端（www.doubao.com）与同一套登录态
// （Cookie / sessionid）。唯一区别是：豆包工作账号带有积分额度（5 小时 / 7 天额度），
// 走账号 Cookie 鉴权，不消耗火山方舟付费 API Key。
//
// 因此本 provider 直接复用 doubao.go（网页版）的 Cookie 鉴权与 SSE 翻译逻辑，仅以独立的
// provider 名（doubao）隔离账号槽位，便于单独登录豆包工作账号。
//
// 与「火山方舟 Ark 付费 API」的区别：Ark 用 sk-... 密钥、按量计费；本实现用登录 Cookie，
// 扣的是你豆包工作账号里的积分，不额外花钱。
//
// 接入方式：login doubao <名称>，粘贴豆包工作客户端登录态 Cookie（sessionid 等），
// 与网页版 doubao 的登录方式完全一致。
package upstream

// doubaoWorkProvider 复用豆包网页版的 Cookie 鉴权协议，作为独立的 doubao 账号槽位。
// 通过嵌入 doubaoProvider 继承其全部方法（ChatStream / StreamTo / Aggregate / RefreshToken /
// FetchModels / StaticModels / Classify），仅重写 Name() 以区分 provider。
type doubaoWorkProvider struct {
	doubaoProvider
}

// Name 以 doubao 注册，与网页版 doubao 隔离账号槽位。
func (p doubaoWorkProvider) Name() string { return "doubao" }
