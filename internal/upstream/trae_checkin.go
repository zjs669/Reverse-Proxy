// trae_checkin.go Trae 国内版每日签到。
//
// 端点来源：客户端 bundle（D:\AI\TraeAi\resources\app\out\main.js）
// 的 iCubeEntitlementManagement.mb()，不是猜的：
//
//	await this.mb("/trae/api/v2/ug/checkin_credits/status", "POST", "checkin_status", void 0, e)
//	await this.mb("/trae/api/v2/ug/checkin_credits/claim",  "POST", "checkin_claim",  void 0, i)
//
// mb() 的前置条件（决定这里为什么国际版直接 Unsupported）：
//
//	if (this.X.providerCode !== wn.CN || c?.account.scope !== wt.MARSCODE)
//	    throw new Error("Checkin credits is unavailable")
//
// 请求体固定 {req_source: 1|2}（IDE=1 / Lite=2），认证头 Cloud-IDE-JWT。
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

const (
	traeCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	traeCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
)

// traeCheckinReqSource IDE=1 / Lite=2。网关按 IDE 口径发。
const traeCheckinReqSource = 1

// traeCheckinDeviceID claim 必需的 x-device-id。
//
// 实测（2026-11-06）：**必须是纯数字**，不能是 UUID。
//   空串 / 缺失        → 9004 "The submitted order parameters are incorrect"
//   UUID（带横线）      → 9074 "当前参与用户太多"（看起来像限流，其实是格式不对）
//   纯数字 498551588155017 → {"code":0,"message":"success"}
//
// 这个值在客户端里是设备指纹，服务端似乎不校验与账号的绑定关系
// （同一个值在多个账号上都能签成功），所以固定一个即可。
const traeCheckinDeviceID = "498551588155017"

// traeCheckinSoftBusyCode 上游「当前参与用户太多」的码。
//
// 修正（2026-11-06 实测）：这个码**不一定是真限流**。
//   x-device-id 用 UUID 格式（带横线）→ 9074
//   x-device-id 用纯数字          → code:0 success
// 所以 9074 更像是「设备标识不合规」，真限流只是它的一种成因。
const traeCheckinSoftBusyCode = 9074

// traeCheckinResp status 与 claim 共用同一形状。
type traeCheckinResp struct {
	Code          int    `json:"code"`
	Message       string `json:"message"`
	Enable        bool   `json:"enable"`
	CheckedIn     bool   `json:"checked_in"`
	DidCheckedIn  bool   `json:"did_checked_in"`
	Credits       int64  `json:"credits"`
	ExtraCredits  int64  `json:"extra_credits"`
}

// Checkin 实现 Trae 签到。国际版无签到体系（额度列表只有 Free plan，无 group_type=1 签到包）。
func (p *traeProvider) Checkin(c *Client, a *auth.Auth) (CheckinResult, error) {
	if p.isIntl(a) {
		return CheckinResult{State: CheckinUnsupported, Msg: "国际版没有签到包"}, nil
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return CheckinResult{State: CheckinFailed, Msg: "账号没有 accessToken"}, nil
	}

	// 先探状态：已签就直接返回 Already，不再打 claim（避免无谓的风控暴露）。
	st, err := p.checkinCall(c, a, traeCheckinStatusPath)
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	if st.Code != 0 {
		return CheckinResult{State: CheckinFailed, Msg: st.Message}, nil
	}
	if st.CheckedIn {
		return CheckinResult{State: CheckinAlready, Msg: "今天已签到", Credit: st.Credits}, nil
	}
	if !st.Enable {
		return CheckinResult{State: CheckinUnsupported, Msg: "签到未开启"}, nil
	}

	cl, err := p.checkinCall(c, a, traeCheckinClaimPath)
	if err != nil {
		return CheckinResult{State: CheckinFailed, Msg: err.Error()}, err
	}
	switch {
	case cl.Code == 0:
		credit := cl.ExtraCredits
		if credit == 0 {
			credit = cl.Credits
		}
		return CheckinResult{State: CheckinOK, Credit: credit, Msg: cl.Message}, nil
	case cl.Code == traeCheckinSoftBusyCode:
		// 9074 实测多为 x-device-id 格式不对（UUID 带横线）而非真限流。
		// 修好头之后仍可能出现，那时才是上游瞬时抖动 —— 归 Failed 让日志能看见。
		return CheckinResult{State: CheckinFailed, Msg: "x-device-id 可能不合规或上游限流：" + cl.Message}, nil
	default:
		return CheckinResult{State: CheckinFailed, Msg: cl.Message}, nil
	}
}

// checkinCall 发一次签到相关请求并解包。HTTP 非 2xx 视为错误，业务 code 交给调用方判断。
func (p *traeProvider) checkinCall(c *Client, a *auth.Auth, path string) (traeCheckinResp, error) {
	var out traeCheckinResp
	body := map[string]any{"req_source": traeCheckinReqSource}
	raw, err := json.Marshal(body)
	if err != nil {
		return out, err
	}
	// 签到接口挂在 api.trae.cn（= refreshBase），与对话域 trae-api-cn.mchost.guru 不同。
	// 实测确认：POST api.trae.cn/trae/api/v2/ug/checkin_credits/status 返回 enable/checked_in。
	req, err := http.NewRequest(http.MethodPost, p.refreshBase(a)+path, bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	p.applyChatHeaders(req, a, "")
	// x-device-id 是 claim 的硬性要求，缺失/格式不对会返回 9004 或伪装成限流的 9074。
	req.Header.Set("x-device-id", traeCheckinDeviceID)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return out, &Error{Kind: Classify(resp.StatusCode, string(rawResp)), Status: resp.StatusCode, Msg: truncate(string(rawResp), 200)}
	}
	if err := json.Unmarshal(rawResp, &out); err != nil {
		return out, fmt.Errorf("签到响应解析失败: %w (body: %s)", err, truncate(string(rawResp), 120))
	}
	return out, nil
}
