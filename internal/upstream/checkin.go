// checkin.go 多供应商每日签到（可选能力）。
//
// 为什么不继续挂在 Client.DailyCheckin 上：签到是**分供应商语义**的。
// 各家的路径、认证头、返回码、幂等表现全不同，硬编码在 Client 上只能服务 CodeBuddy，
// 其他家要么漏签、要么每天刷一堆假错误日志（实测见 docs/checkin-plan.md）。
//
// 设计要点：**「今天已签」不是错误**。上游普遍用同一个业务 code 表示「已签到」
// 和「活动未开启」，只能靠 msg 区分；都当失败会让日志彻底失去信号。
package upstream

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"reverse-proxy/internal/auth"
)

// CheckinState 一次签到的结果状态。调用方按状态决定日志级别：
// OK/Already 正常输出，Pending/Unsupported 静默，Failed 才 WARN。
type CheckinState int

const (
	// CheckinOK 本次刚签到成功。
	CheckinOK CheckinState = iota
	// CheckinAlready 今天已经签过（幂等）。默认时点有 09:00/21:00 两趟，
	// 第二趟必然是这个状态，不能当失败。
	CheckinAlready
	// CheckinUnsupported 该供应商/该区域确认没有签到体系。静默跳过，不刷日志。
	CheckinUnsupported
	// CheckinPending 签到体系存在但端点未接入完成（等抓包确认）。
	// 与 Unsupported 区分开：前者是「在做」，后者是「没有」。
	CheckinPending
	// CheckinFailed 真失败（网络/协议/上游 5xx）。
	CheckinFailed
)

func (s CheckinState) String() string {
	switch s {
	case CheckinOK:
		return "ok"
	case CheckinAlready:
		return "already"
	case CheckinUnsupported:
		return "unsupported"
	case CheckinPending:
		return "pending"
	default:
		return "failed"
	}
}

// CheckinResult 一次签到的结果。
type CheckinResult struct {
	State      CheckinState
	Credit     int64  // 本次到账额度（拿不到为 0）
	StreakDays int    // 连签天数（拿不到为 0）
	Msg        string // 上游原文或说明，写日志/前端展示用
}

// Checkin 执行一次每日签到，按账号 provider 分派。
//
// 未实现 Checkin 的 provider（豆包 / DeepSeek）返回 CheckinUnsupported 而非错误——
// 「这家没有签到」是正常状态，不是故障。
func (c *Client) Checkin(a *auth.Auth) (CheckinResult, error) {
	if up, ok := c.providerFor(a).(interface {
		Checkin(*Client, *auth.Auth) (CheckinResult, error)
	}); ok {
		return up.Checkin(c, a)
	}
	return CheckinResult{State: CheckinUnsupported, Msg: "该供应商没有签到体系"}, nil
}

// ─── CodeBuddy / WorkBuddy ──────────────────────────────────────────────

// codebuddyCheckinStatusPath 签到状态查询（实测可用，此前未接入）。
//
// 注意 active 字段的语义陷阱：国内版 active=false 时 daily-checkin 依然能签成功，
// 它表示「签到活动页是否上线」而不是「今天能否签到」。所以**不能用 active 判能否签到**。
const codebuddyCheckinStatusPath = "/v2/billing/meter/checkin-status"

// Checkin 实现 CodeBuddy / WorkBuddy 的签到。
//
// 国际版（codebuddy.ai）实测无签到活动：daily-checkin 返回
// {"code":10001,"msg":"签到活动未开启或已过期"}，checkin-status 也 active=false。
// 直接返回 Unsupported，不发请求——否则三个国际号每天两趟就是六条假错误日志。
func (p codebuddyProvider) Checkin(c *Client, a *auth.Auth) (CheckinResult, error) {
	if a == nil {
		return CheckinResult{State: CheckinFailed, Msg: "账号为空"}, nil
	}
	if isIntlDomain(a.Domain) {
		return CheckinResult{State: CheckinUnsupported, Msg: "国际版没有签到活动"}, nil
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return CheckinResult{State: CheckinFailed, Msg: "账号没有 accessToken"}, nil
	}

	raw, err := c.billingJSON(a, http.MethodPost, dailyCheckinPath, map[string]any{})
	if err != nil {
		// doJSON 在业务 code != 0 时也返回 error，而「已签到」「活动未开启」
		// 正是这种形态，所以先看错误文案再决定状态。
		if res, ok := classifyCheckinBizErr(err); ok {
			return res, nil
		}
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}

	var out struct {
		Credit     int64 `json:"credit"`
		StreakDays int   `json:"streak_days"`
	}
	_ = json.Unmarshal(raw, &out)
	return CheckinResult{
		State:      CheckinOK,
		Credit:     out.Credit,
		StreakDays: out.StreakDays,
		Msg:        "OK",
	}, nil
}

// classifyCheckinBizErr 从 billingJSON 的错误里识别「今天已签」「活动未开启」。
//
// 这两者上游都用 code 10001 表示，只能靠 msg 区分：
//
//	今天已签到，请明天再来   → Already（幂等，正常）
//	签到活动未开启或已过期   → Unsupported（该区域没活动）
//
// 返回 ok=false 表示不是这两种，调用方按真失败处理。
func classifyCheckinBizErr(err error) (CheckinResult, bool) {
	if err == nil {
		return CheckinResult{}, false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "已签到"):
		return CheckinResult{State: CheckinAlready, Msg: "今天已签到"}, true
	case strings.Contains(msg, "未开启"), strings.Contains(msg, "已过期"):
		return CheckinResult{State: CheckinUnsupported, Msg: "签到活动未开启"}, true
	}
	return CheckinResult{}, false
}

// CheckinStatus 查 CodeBuddy 签到状态（管理台展示用，不影响签到本身）。
func (p codebuddyProvider) CheckinStatus(c *Client, a *auth.Auth) (map[string]any, error) {
	raw, err := c.billingJSON(a, http.MethodPost, codebuddyCheckinStatusPath, map[string]any{})
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("签到状态解析失败: %w", err)
	}
	return m, nil
}

// ─── 额度口径 ────────────────────────────────────────────────────────────

// CreditsInt64 把任意供应商的额度快照折算成整数余额，供 pool.SetCredits /
// ReenableIfCredits 使用。
//
// **必须统一在这里换算**，不能各调用点自己 int64()：
//   - 积分口径（codebuddy / trae 国内 / qoder）→ 原样取整（5200 → 5200）
//   - 美元口径（trae 国际，Unit=="USD"）→ 换成美分（$0.99 → 99）
//
// 直接 int64(0.99) = 0，pool 会判成零余额：权重垫底，
// 且 ReenableIfCredits（要求 remain > 0）永远救不回来。
func CreditsInt64(raw any) int64 {
	switch v := raw.(type) {
	case *TraeUsage:
		if v == nil {
			return 0
		}
		if v.Unit == "USD" {
			return int64(math.Round(v.Remain * 100))
		}
		return int64(v.Remain)
	case *CodeBuddyUsage:
		if v == nil {
			return 0
		}
		return v.Remain
	case *QoderUsage:
		if v == nil {
			return 0
		}
		// Qoder 把额度拆成套餐 + 加油包，实际可用是两者剩余之和。
		return int64(v.UserQuota.Remaining + v.AddOnQuota.Remaining)
	}
	return 0
}
