// admin_deps.go 把网关能力注入 Web 管理台（docs/admin-console.md）。
//
// 为什么放在 cmd/server 而不是 internal/admin：admin 包不 import pool/scheduler/upstream，
// 靠这里的闭包注入具体实现，避免包间循环依赖，也让 admin 可以单测。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"reverse-proxy/internal/admin"
	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/logfmt"
	"reverse-proxy/internal/pool"
	"reverse-proxy/internal/provision"
	"reverse-proxy/internal/scheduler"
	"reverse-proxy/internal/upstream"
)

// cfgWriter 配置文件的安全回写：读全量 → 合并 → normalize 校验 → 原子替换。
//
// 为什么必须走 normalize：config.go 里有重复密钥检测、region 合法性、provider 白名单。
// 绕过它就可能存进一份"网关重启即 fatal"的配置，那比不改还糟。
type cfgWriter struct {
	mu   sync.Mutex
	path string // config.json 路径；空 = 不支持回写
}

// readRaw 读磁盘上的原始 JSON（不含默认值），用于 merge。
func (w *cfgWriter) readRaw() (map[string]any, error) {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		return nil, fmt.Errorf("读取 config.json 失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("config.json 不是合法 JSON: %w", err)
	}
	return m, nil
}

// merge 把 patch 的顶层键覆盖进 base（浅合并，够用且行为可预测）。
func merge(base, patch map[string]any) map[string]any {
	for k, v := range patch {
		base[k] = v
	}
	return base
}

// writeValidated 写回并校验。
//
// 流程：
//  1. 合并后的 JSON 先序列化到临时文件
//  2. 用 Load(临时文件) 跑一遍完整解析 + normalize
//     —— 校验失败就把临时文件删掉，磁盘上的正式配置一个字都没动
//  3. 校验通过才 rename 覆盖正式文件
//
// 这样"存进非法配置导致网关下次启动 fatal"的可能性被彻底消除。
func (w *cfgWriter) writeValidated(m map[string]any) (*Config, error) {
	if w.path == "" {
		return nil, fmt.Errorf("该部署未指定配置文件路径，不支持在线修改")
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := w.path + ".admin.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("写临时文件失败: %w", err)
	}
	// 校验：Load 会 applyEnv + normalize，任何非法值都在这里暴露
	cfg, err := Load(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("配置校验失败（未写入）：%w", err)
	}
	if err := os.Rename(tmp, w.path); err != nil {
		// 单文件 bind mount（docker-compose 的 ./config.json:/app/config.json）下，
		// rename 覆盖挂载点会 EBUSY（device or resource busy）——login 的
		// ensureAccountKey 早就为此改成原地重写，管理台这里走了同一个坑。
		// 回退：内容已校验通过，原地截断重写；失败则用旧内容回滚，不留下半文件。
		if werr := inPlaceWrite(w.path, out); werr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("替换 config.json 失败: %v（原地重写也失败: %v）", err, werr)
		}
		_ = os.Remove(tmp)
	}
	return cfg, nil
}

// inPlaceWrite 原地截断重写文件（不经过 rename，对 bind mount 安全）。
// 写入失败时尝试回滚旧内容：宁可保留修改前的配置，也不要留下半截 JSON
// （网关重启时 Load 失败会直接 fatal）。
func inPlaceWrite(path string, data []byte) error {
	old, _ := os.ReadFile(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		if old != nil {
			_ = os.WriteFile(path, old, 0o644)
		}
		return err
	}
	return f.Close()
}

// isAuthFailure 判断额度查询失败是否属于「令牌过期 / 无权限」这类可直接续期修复的错误。
// 只用于编排层决定要不要自动续期重试，不参与对外错误分类。
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "http 401") || strings.Contains(s, "http 403")
}

// buildAdminDeps 组装管理台依赖。
//
// onReload 在配置变更校验通过后被调用，用于热替换 Handler 的配置快照。
// checkinConcurrency 一键签到的并发度上限。
//
// 为什么要有上限：各账号是独立上游请求，全并发确实最快，但同一供应商多号
// 同时打签到接口极易触发风控（trae 的 claim 已实测会返回
// "当前参与用户太多，请稍后再试"）。4 是折中：10 个号也能从 ~10×RTT 降到 ~3×RTT，
// 又不至于把上游打出限流。
const checkinConcurrency = 4

func buildAdminDeps(
	p *pool.Pool,
	up *upstream.Client,
	sch *scheduler.Scheduler,
	ring *logRingHandle,
	writer *cfgWriter,
	cfgPath string,
	onReload func(*Config),
) admin.Deps {
	started := time.Now()

	// toDTO 把池内状态转成管理台视图。**凭证字段一律不进去**。
	toDTO := func(st pool.Status, label, acctKey string) admin.AccountDTO {
		a := p.AuthByUID(st.UID)
		now := time.Now().Unix()
		remain := int64(0)
		if a != nil && a.ExpiresAt > 0 {
			remain = a.ExpiresAt - now
		}
		renewable := false
		if a != nil {
			prov := a.ProviderOrDefault()
			renewable = prov == "trae" || prov == "qoder" || strings.TrimSpace(a.RefreshToken) != ""
		}
		// 区域修正：pool.Status.Region 来自 auth.Region()，而那个函数只认
		// domain 里的 codebuddy.ai，对 trae 恒返回 "cn"。
		// trae 国际版必须靠 extra.region 判定，否则管理台会把 trae.ai 账号
		// 显示成「国内」，排查时完全误导。
		region := st.Region
		if a != nil && a.ProviderOrDefault() == "trae" {
			if r := strings.ToLower(strings.TrimSpace(a.ExtraVal("region"))); r != "" {
				region = r
			} else if strings.Contains(strings.ToLower(a.Domain), "trae.ai") {
				region = "intl"
			}
		}
		dto := admin.AccountDTO{
			UID: st.UID, Label: label, Nickname: st.Nickname,
			Provider: st.Provider, Region: region,
			Cooling: st.Cooling, CoolKind: st.CoolKind, CoolRemainSec: st.CoolRemaining,
			Disabled: st.Disabled, DisabledReason: st.DisabledReason,
			SuccessCount: st.SuccessCount, ErrTotal: st.ErrTotal,
			InFlight: st.InFlight, BreakerFails: st.BreakerFails,
			AccountKey: acctKey, Renewable: renewable, RemainSeconds: remain, Credits: st.Credits,
		}
		// 额度单位：trae 国际版按美元计费（Credits 存美分），其余都是「积分」。
		// 只在能确定时标 USD，避免把国内版 5200 积分误标成美元。
		if st.Provider == "trae" && region == "intl" {
			dto.CreditsUnit = "USD"
		}
		if a != nil {
			dto.ExpiresAt = a.ExpiresAt
		}
		if !st.LastSuccessTime.IsZero() {
			dto.LastSuccess = st.LastSuccessTime.Unix()
		}
		if !st.LastErrTime.IsZero() {
			dto.LastErr = st.LastErrTime.Unix()
		}
		return dto
	}

	// accountKeysOf 读 config.json 的 account_keys，反查某 uid 的专属密钥与标签。
	accountKeysOf := func() (map[string]string, map[string]string) {
		byUID := map[string]string{}   // uid → secret
		labelOf := map[string]string{} // uid → label
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return byUID, labelOf
		}
		var c Config
		if json.Unmarshal(raw, &c) != nil {
			return byUID, labelOf
		}
		for label, m := range c.AccountKeys {
			for uid, secret := range m {
				byUID[uid] = secret
				labelOf[uid] = label
			}
		}
		return byUID, labelOf
	}

	// v 负责建号编排（写 auth.json → 分配专属密钥 → 同步账号池），
	// 注入给 admin 的 AddAccount / StartOAuth；DeleteAcct 也复用它做即时同步。
	v := &provisioner{p: p, writer: writer, cfgPath: cfgPath, onReload: onReload}

	return admin.Deps{
		Uptime: func() time.Duration { return time.Since(started) },
		// 不能用 CountsDetailed：它等价于 CountsDetailedIn("","")，而空 provider
		// 在 pool.providerMatch 里按历史兼容只统计 codebuddy —— 会把 trae/qoder/
		// doubao/deepseek 全漏掉，管理台显示"总数 0"但实际有号。
		// 这里用 ListAll 自己数，与 /status 的全量口径一致。
		PoolCounts: func() (total, healthy, cooling, disabled, inFlight int) {
			for _, st := range p.ListAll() {
				total++
				switch {
				case st.Disabled:
					disabled++
				case st.Cooling:
					cooling++
				default:
					healthy++
				}
			}
			return
		},
		RedisMode:  func() string { return "" }, // 由 cmd/server 覆盖
		StickyCount: func() int { return 0 },    // 由 cmd/server 覆盖

		ListAccounts: func() []admin.AccountDTO {
			keys, labels := accountKeysOf()
			stat := p.ListAll() // ListAll 才包含 trae/qoder/doubao/deepseek
			out := make([]admin.AccountDTO, 0, len(stat))
			for _, st := range stat {
				label := labels[st.UID]
				if label == "" {
					label = logfmt.UID8(st.UID)
				}
				out = append(out, toDTO(st, label, keys[st.UID]))
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
			return out
		},

		// 身份自检与纠正：只动 extra.cosyUID，不动 uid（池主键）。
		//
		// 为什么需要：qoder 的 uid 同时是 COSY 身份与 cosy-user 头。历史 PAT 导入
		// 在 userinfo 解析时漏了顶层 id，退化成 16 位随机值，于是该号 /algo 链路
		// （模型目录/对话/签到）稳定 403 105，但凭证本身完全有效——界面看不出原因。
		IdentityOf: func(uid string) (admin.IdentityDTO, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return admin.IdentityDTO{}, fmt.Errorf("账号 %s 不在池内", uid)
			}
			res, err := up.CheckIdentity(a)
			if err != nil {
				return admin.IdentityDTO{}, err
			}
			return toIdentityDTO(*res), nil
		},
		AdoptIdentity: func(uid, realUID string) (admin.IdentityDTO, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return admin.IdentityDTO{}, fmt.Errorf("账号 %s 不在池内", uid)
			}
			res, err := up.SetCosyUID(a, realUID)
			if err != nil {
				return admin.IdentityDTO{}, err
			}
			return toIdentityDTO(*res), nil
		},

		AccountByUID: func(uid string) (*admin.AccountDTO, bool) {
			st, ok := p.Status(uid)
			if !ok {
				return nil, false
			}
			keys, labels := accountKeysOf()
			label := labels[uid]
			if label == "" {
				label = logfmt.UID8(uid)
			}
			dto := toDTO(st, label, keys[uid])
			return &dto, true
		},

		ModelsOf: func(uid string) ([]string, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return nil, fmt.Errorf("账号不存在")
			}
			infos, err := up.FetchModels(a)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(infos))
			for _, m := range infos {
				ids = append(ids, m.ID)
			}
			sort.Strings(ids)
			return ids, nil
		},

		// UsageOf 查单号额度：只有 qoder 实现了上游 Usage 接口，其余 provider 的
		// Client.Usage 返回 (nil, nil)——转成 supported=false + hint，让前端提示
		// 「这家上游没有额度接口」，而不是把能力缺失报成故障。
		UsageOf: func(uid string) (admin.UsageDTO, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return admin.UsageDTO{}, fmt.Errorf("账号不存在")
			}
			prov := a.ProviderOrDefault()
			raw, err := up.Usage(a)
			if err != nil && prov == "trae" && isAuthFailure(err) {
				// Trae 的 JWT 只有 8h：刚好过期时额度接口会 401。先续期再重试一次，
				// 否则用户只看到「查积分失败」，不知道其实点下续期就好了。
				if rerr := up.RefreshToken(a); rerr == nil {
					_ = a.SaveAtomic()
					raw, err = up.Usage(a)
				}
			}
			if err != nil {
				return admin.UsageDTO{}, err
			}
			q, ok := raw.(*upstream.QoderUsage)
			if !ok {
				// CodeBuddy / WorkBuddy：billing meter 聚合口径（与 cmd/credit 同源）。
				if cb, ok2 := raw.(*upstream.CodeBuddyUsage); ok2 {
					pct := 0.0
					if cb.Total > 0 {
						pct = float64(cb.Used) / float64(cb.Total) * 100
					}
					p.SetCredits(uid, cb.Remain)
					plan := ""
					if cb.Packages > 0 {
						plan = fmt.Sprintf("%d 个套餐", cb.Packages)
					}
					return admin.UsageDTO{
						Supported: true,
						Provider:  prov,
						Remaining: float64(cb.Remain),
						Total:     float64(cb.Total),
						Used:      float64(cb.Used),
						Unit:      "积分",
						Percent:   pct,
						PlanName:  plan,
					}, nil
				}
				// Trae：独立积分体系，与网页「用量管理」同源（total - consumed）。
				if tr, ok2 := raw.(*upstream.TraeUsage); ok2 {
					unit := tr.Unit
					if unit == "" {
						unit = "积分"
					}
					// 余额写回 pool 时按单位换算成整数：
					//   - 「积分」原样取整（5200 分 → 5200）
					//   - USD 换成美分（$0.99 → 99）
					// 不能一律 int64(remain)：国际版余额是 $0.99 这种量级，
					// 截断后变成 0，pool 会把它当成零余额——权重垫底，且
					// ReenableIfCredits（签到解冻要求 remain > 0）永远救不回来。
					balance := int64(tr.Remain)
					if unit == "USD" {
						balance = int64(math.Round(tr.Remain * 100))
					}
					p.SetCredits(uid, balance)
					return admin.UsageDTO{
						Supported: true,
						Provider:  prov,
						Remaining: tr.Remain,
						Total:     tr.Total,
						Used:      tr.Used,
						Unit:      unit,
						Percent:   tr.Ratio * 100,
						PlanName:  tr.PlanName,
					}, nil
				}
				hint := prov + " 上游没有积分/额度接口"
				// 把「为什么查不到」说清楚：豆包 / DeepSeek 是网页会话接口，没有积分概念
				switch prov {
				case "doubao", "deepseek":
					hint = prov + " 为网页接口，没有积分概念"
				}
				return admin.UsageDTO{
					Supported: false,
					Provider:  prov,
					Hint:      hint,
				}, nil
			}
			remain := q.UserQuota.Remaining + q.AddOnQuota.Remaining
			total := q.UserQuota.Total + q.AddOnQuota.Total
			used := q.UserQuota.Used + q.AddOnQuota.Used
			// 回写池内额度：选号权重与 /status 的 credits 字段同步用上
			p.SetCredits(uid, int64(remain))
			return admin.UsageDTO{
				Supported: true,
				Provider:  prov,
				Remaining: remain,
				Total:     total,
				Used:      used,
				Unit:      q.UserQuota.Unit,
				Percent:   q.TotalUsagePct,
				PlanName:  q.PlanTierName,
			}, nil
		},

		// RenewOne 与 token-watch 走同一条路（upstream.RefreshToken），
		// 成功后写回 auth.json 并把池状态复位。
		RenewOne: func(uid string) (int64, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return 0, fmt.Errorf("账号不存在")
			}
			if err := up.RefreshToken(a); err != nil {
				return 0, err
			}
			if err := a.SaveAtomic(); err != nil {
				return 0, fmt.Errorf("续期成功但写回失败: %w", err)
			}
			p.ClearSessionDead(uid)
			p.ReviveIfCredDead(uid)
			return a.ExpiresAt, nil
		},

		RenewAll: func() error { sch.RunTokenWatchNow(); return nil },

		ReviveOne: func(uid string) error {
			if _, ok := p.Status(uid); !ok {
				return fmt.Errorf("账号不存在")
			}
			p.ReviveDisabled(uid)
			p.ClearSessionDead(uid)
			return nil
		},

		// ResetKey 生成新密钥，且**保证不与任何已有密钥冲突**
		// （config.go 的 normalize 会报"账户 key 必须唯一"）。
		ResetKey: func(uid string) (string, error) {
			if _, ok := p.Status(uid); !ok {
				return "", fmt.Errorf("账号不存在")
			}
			raw, err := writer.readRaw()
			if err != nil {
				return "", err
			}
			current, _, err := readKeysFromRaw(raw)
			if err != nil {
				return "", err
			}
			// 找该 uid 所在的标签
			label := ""
			for l, m := range current.AccountKeys {
				if _, ok := m[uid]; ok {
					label = l
				}
			}
			if label == "" {
				return "", fmt.Errorf("该账号还没有专属密钥条目，请先在 auth.json 里确认标签")
			}
			used := map[string]bool{current.APIKey: true}
			for k := range current.APIKeys {
				used[k] = true
			}
			for k := range current.ProviderKeys {
				used[k] = true
			}
			for _, m := range current.AccountKeys {
				for _, s := range m {
					used[s] = true
				}
			}
			var fresh string
			for i := 0; i < 8; i++ {
				fresh = newSecret()
				if !used[fresh] {
					break
				}
			}
			if fresh == "" || used[fresh] {
				return "", fmt.Errorf("生成唯一密钥失败，请重试")
			}
			current.AccountKeys[label][uid] = fresh
			if err := writeKeysToRaw(raw, current); err != nil {
				return "", err
			}
			if _, err := writer.writeValidated(raw); err != nil {
				return "", err
			}
			return fresh, nil
		},

		// DeleteAcct 从 auth.json 移除该账号（连带清掉 account_keys 条目）。
		DeleteAcct: func(uid string) error {
			af := authFilePath(cfgPath)
			auths, err := auth.LoadFile(af)
			if err != nil {
				return err
			}
			var target *auth.Auth
			for _, a := range auths {
				if a.UID == uid {
					target = a
					break
				}
			}
			if target == nil {
				return fmt.Errorf("账号不存在")
			}
			if target.FileKey == "" {
				return fmt.Errorf("该账号不是单文件模式，无法在线删除")
			}
			if err := removeAuthEntry(af, target.FileKey); err != nil {
				return err
			}
			// 顺带清 account_keys，避免残留一把仍能过鉴权的孤儿密钥
			raw, err := writer.readRaw()
			if err == nil {
				cur, _, e2 := readKeysFromRaw(raw)
				if e2 == nil {
					for label, m := range cur.AccountKeys {
						if _, ok := m[uid]; ok {
							delete(m, uid)
							if len(m) == 0 {
								delete(cur.AccountKeys, label)
							}
						}
					}
					if writeKeysToRaw(raw, cur) == nil {
						_, _ = writer.writeValidated(raw)
					}
				}
			}
			v.sync() // 删号立即生效（watchAuths 的 5s 轮询只是兜底）
			return nil
		},

		ReadKeys: func() (admin.KeysDTO, error) {
			raw, err := writer.readRaw()
			if err != nil {
				return admin.KeysDTO{}, err
			}
			k, _, err := readKeysFromRaw(raw)
			return k, err
		},

		WriteKeys: func(k admin.KeysDTO) error {
			raw, err := writer.readRaw()
			if err != nil {
				return err
			}
			if err := writeKeysToRaw(raw, k); err != nil {
				return err
			}
			cfg, err := writer.writeValidated(raw)
			if err != nil {
				return err
			}
			if onReload != nil {
				onReload(cfg)
			}
			return nil
		},

		// AddAccount 粘贴型建号（trae/qoder/doubao/deepseek）：
		// 归一凭证 → 写 auth.json → 分配专属密钥 → 立即入池。
		AddAccount: func(req admin.AddAccountReq) (admin.AddAccountResult, error) {
			provider, region, err := addProvider(req.Provider)
			if err != nil {
				return admin.AddAccountResult{}, err
			}
			if provider == "codebuddy" {
				return admin.AddAccountResult{}, fmt.Errorf("CodeBuddy 需要浏览器授权（region=%s）：请用「授权链接」方式添加", region)
			}
			cred, err := provision.FromPasteRegion(provider, region, req.Input, req.Nickname)
			if err != nil {
				return admin.AddAccountResult{}, err
			}
			return v.save(cred)
		},

		// StartOAuth CodeBuddy 设备流：返回可轮询的句柄，后台每 3s 问一次上游。
		StartOAuth: func(providerID, nickname string) (admin.OAuthFlow, error) {
			provider, region, err := addProvider(providerID)
			if err != nil {
				return nil, err
			}
			if provider != "codebuddy" {
				return nil, fmt.Errorf("%s 不需要授权链接：直接在「添加账号」里粘凭证即可", provider)
			}
			sess, err := provision.StartCodeBuddy(region)
			if err != nil {
				return nil, err
			}
			return &oauthFlow{sess: sess, nickname: nickname, v: v}, nil
		},

		ReadSettings: func() (map[string]any, error) {
			raw, err := writer.readRaw()
			if err != nil {
				return nil, err
			}
			// 密钥类整段剔除：密钥在「密钥」页管，设置页不该顺手暴露
			delete(raw, "api_key")
			delete(raw, "api_keys")
			delete(raw, "provider_keys")
			delete(raw, "account_keys")
			// admin.password 是登录口令，回显到页面上等于把管理台钥匙贴在门口。
			// 只保留是否设置过，改口令请直接改 config.json。
			if adm, ok := raw["admin"].(map[string]any); ok {
				if pw, has := adm["password"].(string); has {
					if pw != "" {
						adm["password_set"] = true
					}
					delete(adm, "password")
				}
			}
			return raw, nil
		},

		WriteSettings: func(patch map[string]any) error {
			raw, err := writer.readRaw()
			if err != nil {
				return err
			}
			// 设置页不允许改密钥（那些走 /keys，有自己的唯一性校验）
			delete(patch, "api_key")
			delete(patch, "api_keys")
			delete(patch, "provider_keys")
			delete(patch, "account_keys")
			// password_set 只是只读标记，不是真配置项，写回前清掉，
			// 否则下次 Load 会因为它是个未知键而无害但脏地留在文件里。
			if adm, ok := patch["admin"].(map[string]any); ok {
				delete(adm, "password_set")
			}
			merged := merge(raw, patch)
			cfg, err := writer.writeValidated(merged)
			if err != nil {
				return err
			}
			if onReload != nil {
				onReload(cfg)
			}
			return nil
		},

		CurrentAPIKey: func() string {
			raw, err := writer.readRaw()
			if err != nil {
				return ""
			}
			s, _ := raw["api_key"].(string)
			return s
		},

		// 签到：分供应商语义（upstream.Checkin 按 provider 分派）。
		// 「今天已签」「没有签到体系」「接口待接入」都不是错误，统一返回 state 让前端着色。
		CheckinOne: func(uid string) (admin.CheckinDTO, error) {
			a := p.AuthByUID(uid)
			if a == nil {
				return admin.CheckinDTO{}, fmt.Errorf("账号 %s 不在池内", uid)
			}
			_, labels := accountKeysOf()
			label := labels[uid]
			if label == "" {
				label = logfmt.UID8(uid)
			}
			return toCheckinDTO(uid, label, sch.CheckinOne(a)), nil
		},
		CheckinAll: func() []admin.CheckinDTO {
			// 必须用 ListAll：p.List() 只含 codebuddy，trae/qoder/doubao/deepseek 会被漏掉，
			// 一键签到就成了「只签 codebuddy」。
			_, labels := accountKeysOf()
			stat := p.ListAll()
			type job struct {
				uid   string
				label string
				a     *auth.Auth
			}
			jobs := make([]job, 0, len(stat))
			for _, st := range stat {
				a := p.AuthByUID(st.UID)
				if a == nil {
					continue
				}
				label := labels[st.UID]
				if label == "" {
					label = logfmt.UID8(st.UID)
				}
				jobs = append(jobs, job{uid: st.UID, label: label, a: a})
			}

			// 并发签到：各账号是完全独立的上游请求，串行跑 N 个号要 N × RTT，
			// 10 个号就是十几秒，前端只能干等。这里按号并发。
			//
			// 并发度上限 checkinConcurrency：不能无脑 len(jobs) 全放。
			// 同一供应商多号同时打同一个签到接口很容易触发风控
			// （trae 的 claim 已经在报「当前参与用户太多」），限并发比限速更可控。
			out := make([]admin.CheckinDTO, len(jobs))
			limit := checkinConcurrency
			if limit <= 0 || limit > len(jobs) {
				limit = len(jobs)
			}
			sem := make(chan struct{}, limit)
			var wg sync.WaitGroup
			for i := range jobs {
				wg.Add(1)
				go func(i int, jb job) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					// CheckinOne 内部对 auth.Auth 加锁（RefreshToken 路径），
					// 且每个账号各自独立，不存在跨号共享状态。
					out[i] = toCheckinDTO(jb.uid, jb.label, sch.CheckinOne(jb.a))
				}(i, jobs[i])
			}
			wg.Wait()

			sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
			return out
		},
	}
}

// toCheckinDTO 把 upstream.CheckinResult 转成前端视图。
//
// state 用字符串而不是直接暴露 upstream 枚举：admin 包不 import upstream，
// 前端也只认字符串。
func toCheckinDTO(uid, label string, res upstream.CheckinResult) admin.CheckinDTO {
	return admin.CheckinDTO{
		UID:    uid,
		Label:  label,
		State:  res.State.String(),
		Credit: res.Credit,
		Streak: res.StreakDays,
		Msg:    res.Msg,
	}
}

// toIdentityDTO 把 upstream.QoderIdentity 转成前端视图。
func toIdentityDTO(res upstream.QoderIdentity) admin.IdentityDTO {
	return admin.IdentityDTO{
		AccountUID: res.AccountUID,
		CosyUID:    res.CosyUID,
		RealUID:    res.RealUID,
		Match:      res.Match,
		Err:        res.Err,
	}
}