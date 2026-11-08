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

// stripSecrets 从设置页的 map 里剔除密钥类键（密钥走 /keys 单独管理）。
func stripSecrets(m map[string]any) {
	for _, k := range []string{"api_key", "api_keys", "provider_keys", "account_keys"} {
		delete(m, k)
	}
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

	// mustAuth 取池内账号，不存在时报错。
	mustAuth := func(uid string) (*auth.Auth, error) {
		if a := p.AuthByUID(uid); a != nil {
			return a, nil
		}
		return nil, fmt.Errorf("账号 %s 不在池内", uid)
	}

	// identityBy 跑一次身份探测/纠正，统一前置校验与 DTO 转换。
	identityBy := func(uid string, fn func(*auth.Auth) (*upstream.QoderIdentity, error)) (admin.IdentityDTO, error) {
		a, err := mustAuth(uid)
		if err != nil {
			return admin.IdentityDTO{}, err
		}
		res, err := fn(a)
		if err != nil {
			return admin.IdentityDTO{}, err
		}
		return toIdentityDTO(*res), nil
	}

	// displayLabel 取账号显示名，缺省退回 uid 前 8 位。
	displayLabel := func(labels map[string]string, uid string) string {
		if l := labels[uid]; l != "" {
			return l
		}
		return logfmt.UID8(uid)
	}

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
		// deepseek 全漏掉，管理台显示"总数 0"但实际有号。
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
			stat := p.ListAll() // ListAll 才包含 trae/qoder/deepseek
			out := make([]admin.AccountDTO, 0, len(stat))
			for _, st := range stat {
				label := displayLabel(labels, st.UID)
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
			return identityBy(uid, up.CheckIdentity)
		},
		AdoptIdentity: func(uid, realUID string) (admin.IdentityDTO, error) {
			return identityBy(uid, func(a *auth.Auth) (*upstream.QoderIdentity, error) {
				return up.SetCosyUID(a, realUID)
			})
		},

		AccountByUID: func(uid string) (*admin.AccountDTO, bool) {
			st, ok := p.Status(uid)
			if !ok {
				return nil, false
			}
			keys, labels := accountKeysOf()
			label := displayLabel(labels, uid)
			dto := toDTO(st, label, keys[uid])
			return &dto, true
		},

		ModelsOf: func(uid string) ([]string, error) {
			a, err := mustAuth(uid)
			if err != nil {
				return nil, err
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
			a, err := mustAuth(uid)
			if err != nil {
				return admin.UsageDTO{}, err
			}
			prov := a.ProviderOrDefault()
			raw, err := up.Usage(a)
			// Trae 的 JWT 只有 8h，刚过期时额度接口 401；先续期再试一次。
			if err != nil && prov == "trae" && isAuthFailure(err) {
				if rerr := up.RefreshToken(a); rerr == nil {
					_ = a.SaveAtomic()
					raw, err = up.Usage(a)
				}
			}
			if err != nil {
				return admin.UsageDTO{}, err
			}
			dto := admin.UsageDTO{Supported: true, Provider: prov}
			switch v := raw.(type) {
			case *upstream.QoderUsage:
				remain := v.UserQuota.Remaining + v.AddOnQuota.Remaining
				dto.Remaining, dto.Used = remain, v.UserQuota.Used+v.AddOnQuota.Used
				dto.Total, dto.Unit = v.UserQuota.Total+v.AddOnQuota.Total, v.UserQuota.Unit
				dto.Percent, dto.PlanName = v.TotalUsagePct, v.PlanTierName
				p.SetCredits(uid, int64(remain))
			case *upstream.CodeBuddyUsage:
				dto.Remaining, dto.Total, dto.Used = float64(v.Remain), float64(v.Total), float64(v.Used)
				dto.Unit = "积分"
				if v.Total > 0 {
					dto.Percent = float64(v.Used) / float64(v.Total) * 100
				}
				if v.Packages > 0 {
					dto.PlanName = fmt.Sprintf("%d 个套餐", v.Packages)
				}
				p.SetCredits(uid, v.Remain)
			case *upstream.TraeUsage:
				dto.Remaining, dto.Total, dto.Used = v.Remain, v.Total, v.Used
				dto.Percent, dto.PlanName = v.Ratio*100, v.PlanName
				dto.Unit = v.Unit
				if dto.Unit == "" {
					dto.Unit = "积分"
				}
				// 余额按单位换算：USD 存美分（$0.99→99），否则截断成 0 会让 pool 当零余额。
				balance := int64(v.Remain)
				if dto.Unit == "USD" {
					balance = int64(math.Round(v.Remain * 100))
				}
				p.SetCredits(uid, balance)
			default:
				dto.Supported = false
				dto.Hint = prov + " 为网页接口，没有积分概念"
			}
			return dto, nil
		},

		// RenewOne 与 token-watch 走同一条路（upstream.RefreshToken），
		// 成功后写回 auth.json 并把池状态复位。
		RenewOne: func(uid string) (int64, error) {
			a, err := mustAuth(uid)
			if err != nil {
				return 0, err
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
			raw, current, err := writer.readKeys()
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
			fresh, err := freshSecret(usedSecrets(current))
			if err != nil {
				return "", err
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
			_, k, err := writer.readKeys()
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

		// AddAccount 粘贴型建号（trae/qoder/deepseek）：
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
			stripSecrets(raw)
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
			stripSecrets(patch)
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
			a, err := mustAuth(uid)
			if err != nil {
				return admin.CheckinDTO{}, err
			}
			_, labels := accountKeysOf()
			label := displayLabel(labels, uid)
			return toCheckinDTO(uid, label, sch.CheckinOne(a)), nil
		},
		CheckinAll: func() []admin.CheckinDTO {
			_, labels := accountKeysOf()
			stat := p.ListAll() // p.List() 只含 codebuddy，会漏掉其他 provider
			type job struct{ uid, label string; a *auth.Auth }
			jobs := make([]job, 0, len(stat))
			for _, st := range stat {
				if a := p.AuthByUID(st.UID); a != nil {
					jobs = append(jobs, job{st.UID, displayLabel(labels, st.UID), a})
				}
			}
			out := make([]admin.CheckinDTO, len(jobs))
			limit := checkinConcurrency
			if limit <= 0 || limit > len(jobs) {
				limit = len(jobs)
			}
			// 限并发：同供应商多号同时打签到接口易触发风控（trae 实测）。
			sem := make(chan struct{}, limit)
			var wg sync.WaitGroup
			for i, jb := range jobs {
				wg.Add(1)
				go func(i int, jb job) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					out[i] = toCheckinDTO(jb.uid, jb.label, sch.CheckinOne(jb.a))
				}(i, jb)
			}
			wg.Wait()
			sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
			return out
		},
	}
}

	// toCheckinDTO 转前端视图（state 用字符串，admin 不 import upstream）。
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