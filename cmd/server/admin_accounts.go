// admin_accounts.go 管理台建号：粘贴型凭证入库 + CodeBuddy 设备流。
// 换凭证/归一/落盘共用 internal/provision；本文件只编排（写盘 → 分配密钥 → 同步池）。
package main

import (
	"fmt"
	"log"
	"strings"

	"reverse-proxy/internal/admin"
	"reverse-proxy/internal/auth"
	"reverse-proxy/internal/logfmt"
	"reverse-proxy/internal/pool"
	"reverse-proxy/internal/provision"
)

// addProvider 把管理台的供应商 id 归一成（provider, region），由 region 决定键前缀与端点。
func addProvider(id string) (provider, region string, err error) {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "codebuddy", "codebuddy_cn":
		return "codebuddy", "cn", nil
	case "codebuddy_intl", "codebuddyai":
		return "codebuddy", "intl", nil
	// Trae 国内版 / 国际版是两套独立体系（域名、认证头、积分接口全不同），
	// 必须显式区分；只给 "trae" 时按国内版处理，保持与旧行为兼容。
	case "trae", "trae_cn":
		return "trae", "cn", nil
	case "trae_intl", "traeai":
		return "trae", "intl", nil
// Qoder 国内/国际是两套独立体系（gateway.qoder.com.cn vs api3.qoder.sh，
// 模型目录与签到活动都不通）；只给 "qoder" 按国内处理，兼容旧行为。
	case "qoder", "qoder_cn":
		return "qoder", "cn", nil
	case "qoder_intl", "qoderai":
		return "qoder", "intl", nil
	case "deepseek":
		return strings.ToLower(strings.TrimSpace(id)), "", nil
	}
	return "", "", fmt.Errorf("未知供应商 %q（可选 codebuddy / codebuddy_intl / trae / trae_intl / qoder / qoder_intl / deepseek）", id)
}

// provisioner 建号编排器。
type provisioner struct {
	p        *pool.Pool
	writer   *cfgWriter
	cfgPath  string
	onReload func(*Config)
}

func (v *provisioner) authFile() string { return authFilePath(v.cfgPath) }

// save 落盘并返回管理台视图。cred.Nickname 由调用方填好。
func (v *provisioner) save(cred *provision.Credential) (admin.AddAccountResult, error) {
	label, err := provision.Save(v.authFile(), "", cred)
	if err != nil {
		return admin.AddAccountResult{}, err
	}
	key, err := v.ensureAccountKey(label, cred.UID)
	if err != nil {
		// 账号已经落盘，密钥写失败不该把建号整体判死；但要让运维知道要手工补
		log.Printf("WARN: [admin] 账号 %s 已写入，但生成专属密钥失败：%v", label, err)
	}
	inPool := v.sync()
	log.Printf("[admin] 新增账号 %s（%s/%s）uid=%s 入池=%v",
		label, cred.Provider, cred.Region, logfmt.UID8(cred.UID), inPool)
	return admin.AddAccountResult{
		Label: label, UID: cred.UID, Provider: cred.Provider, AccountKey: key, InPool: inPool,
	}, nil
}

// ensureAccountKey 给新账号分配专属密钥（已存在则沿用），写回 config.json 并热重载。
// 与 admin_deps.go 的 ResetKey 同口径：保证全局唯一，否则 normalize 会拒绝整份配置。
func (v *provisioner) ensureAccountKey(label, uid string) (string, error) {
	raw, current, err := v.writer.readKeys()
	if err != nil {
		return "", err
	}
	if current.AccountKeys[label] == nil {
		current.AccountKeys[label] = map[string]string{}
	}
	if secret := strings.TrimSpace(current.AccountKeys[label][uid]); secret != "" {
		return secret, nil // 续期/重登沿用，不覆盖正在被客户端使用的密钥
	}
	fresh, err := freshSecret(usedSecrets(current))
	if err != nil {
		return "", err
	}
	current.AccountKeys[label][uid] = fresh
	if err := writeKeysToRaw(raw, current); err != nil {
		return "", err
	}
	cfg, err := v.writer.writeValidated(raw)
	if err != nil {
		return "", err
	}
	if v.onReload != nil {
		v.onReload(cfg)
	}
	return fresh, nil
}

// sync 立刻把 auth.json 同步进账号池（watchAuths 的 5s 轮询只是兜底）。
// 返回池里是否已有账号（空池时 false，前端据此提示）。
func (v *provisioner) sync() bool {
	auths, err := auth.LoadFile(v.authFile())
	if err != nil {
		log.Printf("WARN: [admin] 建号后读取 auth.json 失败：%v", err)
		return false
	}
	v.p.SyncToDir(auths)
	return len(auths) > 0
}

// oauthFlow 把 provision 的设备流会话适配成 admin.OAuthFlow：
// 轮询到凭证后在这里完成"写盘 + 密钥 + 入池 + 签到"，admin 只维护任务状态表。
type oauthFlow struct {
	sess     *provision.OAuthSession
	nickname string
	v        *provisioner
}

func (f *oauthFlow) AuthURL() string { return f.sess.AuthURL }

func (f *oauthFlow) Poll() (*admin.AddAccountResult, bool, error) {
	cred, pending, err := f.sess.Poll()
	if err != nil || pending || cred == nil {
		return nil, pending, err
	}
	cred.Nickname = f.nickname
	res, err := f.v.save(cred)
	if err != nil {
		return nil, false, err
	}
	// 建号成功顺手签到一次（幂等）。失败只记日志：签到不是建号的前提条件。
	if ok, msg := provision.Checkin(cred.Domain, cred.AccessToken, cred.UID, cred.EnterpriseID); ok {
		log.Printf("[admin] 新号 %s 签到成功", res.Label)
	} else if msg != "" {
		log.Printf("WARN: [admin] 新号 %s 签到失败：%s", res.Label, msg)
	}
	return &res, false, nil
}
