// codebuddy_usage.go CodeBuddy 积分查询：把 billing meter 的聚合结果接进 Provider.Usage。
//
// 为什么单独一个文件：额度查询是可选能力（Provider.Usage 不是必选接口），
// 放在这里避免污染 codebuddy.go 里主链路（chat/refresh）的阅读路径。
package upstream

import (
	"reverse-proxy/internal/auth"
)

// CodeBuddyUsage 账号积分快照（billing meter 聚合口径）。
type CodeBuddyUsage struct {
	Remain   int64 // 所有套餐剩余之和
	Used     int64
	Total    int64
	Packages int // 有效期内的套餐数
}

// Usage 实现 Provider 的可选额度接口（管理台「额度 / 积分 → 查询额度」用）。
//
// 数据源与 cmd/credit 完全相同：POST {billingBase}/v2/billing/meter/get-user-resource。
// 国内/国际域由 billingBase 按账号 domain 自动选择（codebuddy.cn / codebuddy.ai）。
func (p codebuddyProvider) Usage(c *Client, a *auth.Auth) (any, error) {
	st, err := c.UserResourceDetail(a)
	if err != nil {
		return nil, err
	}
	return &CodeBuddyUsage{
		Remain:   st.Remain,
		Used:     st.Used,
		Total:    st.Total,
		Packages: st.Packages,
	}, nil
}
