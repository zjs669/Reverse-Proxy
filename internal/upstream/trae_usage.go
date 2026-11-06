// trae_usage.go Trae 积分查询：把网页「用量管理」页的额度接口接进 Provider.Usage。
//
// 实测结论（2026-11，浏览器抓包 + 裸 curl 双向验证）：
//   - 端点 POST https://api.trae.cn/trae/api/v2/pay/user_current_entitlement_list
//   - 只需 Authorization: Cloud-IDE-JWT <jwt> + X-Cloudide-Session: <source_id>，
//     **不需要** cookie / msToken / a_bogus 等网页侧风控参数，所以网关可直接复用
//     池里的 accessToken（8h）与 source_id（长期），无需用户再登录一次。
//   - 响应 usage_summary.total_amount / consumed_amount 已是全量聚合口径
//     （含签到、月度赠送、认证奖励），与页面「总可用积分」完全一致：
//     可用 = total_amount - consumed_amount。
//
// 国际版（trae.ai）是**另一套接口**（2026-11-05 实测）：
//   - 端点 POST https://ug-normal.trae.ai/trae/api/v1/pay/user_current_entitlement_list
//   - 认证只需 Authorization: Cloud-IDE-JWT <jwt> 一个头（不要 X-Cloudide-Session）
//   - 路径是 v1 且挂在 ug-normal 独立域，与国内版 v2 挂在 api.trae.cn 不同
//   - 响应没有 usage_summary，额度在 quota.basic_usage_limit 与
//     usage.basic_usage_amount（美元口径，页面显示为 "$0.01 / $1.00"）
//
// 早前管理台把 trae 判成「订阅制、没有积分接口」是误判，实际 Trae 有独立积分体系。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"reverse-proxy/internal/auth"
)

// traeEntitlementPath 额度接口路径（挂在 api.trae.cn，与 GetUserToken 同域）。
const traeEntitlementPath = "/trae/api/v2/pay/user_current_entitlement_list"

// traeIntlEntitlementBase / traeIntlEntitlementPath 国际版额度接口。
// 与国内版三处差异：独立域名（ug-normal）、v1 而非 v2、不需要 X-Cloudide-Session。
const traeIntlEntitlementBase = "https://ug-normal.trae.ai"
const traeIntlEntitlementPath = "/trae/api/v1/pay/user_current_entitlement_list"

// TraeUsage 账号积分快照。
//
// 口径说明：Remain/Used/Total 直接取上游 usage_summary，
// 不要把 user_entitlement_pack_list 自己累加——包里混有过期与已用完的项，
// 自行聚合会与页面数字对不上（页面的「总可用积分」用的就是 usage_summary）。
type TraeUsage struct {
	Remain   float64 // 总可用积分
	Used     float64 // 已消耗
	Total    float64 // 总额
	Ratio    float64 // 已用比例（0~1）
	PlanName string  // 当前方案（如「免费」），取不到为空
	Packages int     // 权益包数量
	// Unit 计量单位：国内版是「积分」；国际版按 is_dollar_usage_billing 可能是「USD」。
	// 不写死，否则管理台会把 $1.00 的美元额度显示成「积分」，看的人会误判成 1 积分。
	Unit string
}

// Usage 实现 Provider 的可选额度接口（管理台「查积分」用）。
//
// 请求头复用 applyChatHeaders：它已包含 Authorization（Cloud-IDE-JWT）、
// X-Cloudide-Session、X-Uid、x-app-id 与桌面端 UA，正是该接口要求的组合；
// 仅把 Accept 从 SSE 覆盖成 JSON。
func (p *traeProvider) Usage(c *Client, a *auth.Auth) (any, error) {
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("trae usage: 账号没有 accessToken，请先重新登录")
	}
	if p.isIntl(a) {
		return p.usageIntl(c, a)
	}
	body := []byte(`{"require_usage":true,"full_data":true}`)
	req, err := http.NewRequest(http.MethodPost, traeRefreshBase+traeEntitlementPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.applyChatHeaders(req, a, "")
	req.Header.Set("Accept", "application/json") // 覆盖 SSE 头，否则上游可能按流式返回

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae usage http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		UsageSummary struct {
			ConsumedAmount float64 `json:"consumed_amount"`
			TotalAmount    float64 `json:"total_amount"`
			Ratio          float64 `json:"consumption_ratio"`
		} `json:"usage_summary"`
		Packs []struct {
			DisplayDesc string `json:"display_desc"`
			Base        struct {
				ProductID   int `json:"product_id"`
				ProductType int `json:"product_type"`
			} `json:"entitlement_base_info"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae usage parse: %w", err)
	}
	remain := out.UsageSummary.TotalAmount - out.UsageSummary.ConsumedAmount
	if remain < 0 {
		remain = 0
	}
	// 方案名：订阅型包（entitlement_base_info.product_id=0 且 product_type=0，如「免费」）。
	// 不能直接取第一个包——列表按业务优先级排序，首项往往是「大学生认证奖励」这类积分包，
	// 不是订阅方案（已踩过一次：管理台把方案名错显成奖励名）。
	plan := ""
	for _, pk := range out.Packs {
		if pk.Base.ProductID == 0 && pk.Base.ProductType == 0 && pk.DisplayDesc != "" {
			plan = pk.DisplayDesc
			break
		}
	}
	return &TraeUsage{
		Remain:   remain,
		Used:     out.UsageSummary.ConsumedAmount,
		Total:    out.UsageSummary.TotalAmount,
		Ratio:    out.UsageSummary.Ratio,
		PlanName: plan,
		Packages: len(out.Packs),
		Unit:     "积分",
	}, nil
}

// usageIntl 国际版额度查询（trae.ai）。
//
// 与国内版的差异（2026-11-05 实测）：
//   - 域名独立：ug-normal.trae.ai，不是 api.trae.ai
//   - 路径是 v1（国内版 v2）
//   - 只认 Authorization: Cloud-IDE-JWT <jwt>，**不要**带 X-Cloudide-Session
//     （带上也不会错，但没必要；实测最小参数就是这一个头）
//   - 响应结构完全不同：没有 usage_summary，额度在
//     entitlement_base_info.quota.basic_usage_limit（上限）
//     与 usage.basic_usage_amount（已用），都是美元口径
//
// 返回同样的 *TraeUsage，让上层（/usage 接口、管理台）不用分区域处理。
func (p *traeProvider) usageIntl(c *Client, a *auth.Auth) (any, error) {
	jwt := strings.TrimSpace(a.AccessToken)
	body := []byte(`{"require_usage":true}`)
	req, err := http.NewRequest(http.MethodPost, traeIntlEntitlementBase+traeIntlEntitlementPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+jwt)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trae intl usage http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var out struct {
		IsDollarBilling bool `json:"is_dollar_usage_billing"`
		Packs           []struct {
			DisplayDesc string `json:"display_desc"`
			Status      int    `json:"status"`
			Base        struct {
				ProductID   int `json:"product_id"`
				ProductType int `json:"product_type"`
				Quota       struct {
					BasicUsageLimit float64 `json:"basic_usage_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				BasicUsageAmount float64 `json:"basic_usage_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("trae intl usage parse: %w", err)
	}

	// 取第一条 status==1（有效）的套餐；没有就退回第一条。
	// 不能盲目累加所有包：列表里混着已过期/已用完的项，自行聚合会与页面数字对不上
	// （与国内版同一条教训，见文件头注释）。
	pick := -1
	for i := range out.Packs {
		if out.Packs[i].Status == 1 {
			pick = i
			break
		}
	}
	if pick < 0 && len(out.Packs) > 0 {
		pick = 0
	}
	if pick < 0 {
		return &TraeUsage{}, nil
	}
	pk := out.Packs[pick]

	// 单位：is_dollar_usage_billing=true 表示额度以美元计（如 $1.00），
	// 否则按积分口径展示。写死「积分」会把 $0.01/$1.00 显示成 0.01/1 积分。
	unit := "积分"
	if out.IsDollarBilling {
		unit = "USD"
	}

	remain := pk.Base.Quota.BasicUsageLimit - pk.Usage.BasicUsageAmount
	if remain < 0 {
		remain = 0
	}
	ratio := 0.0
	if pk.Base.Quota.BasicUsageLimit > 0 {
		ratio = pk.Usage.BasicUsageAmount / pk.Base.Quota.BasicUsageLimit
	}

	// 方案名：订阅型包（product_id=0 且 product_type=0）才是方案本身，
	// 首项往往是奖励包（同国内版的坑）。
	plan := ""
	for _, q := range out.Packs {
		if q.Base.ProductID == 0 && q.Base.ProductType == 0 && q.DisplayDesc != "" {
			plan = q.DisplayDesc
			break
		}
	}

	return &TraeUsage{
		Remain:   remain,
		Used:     pk.Usage.BasicUsageAmount,
		Total:    pk.Base.Quota.BasicUsageLimit,
		Ratio:    ratio,
		PlanName: plan,
		Packages: len(out.Packs),
		Unit:     unit,
	}, nil
}
