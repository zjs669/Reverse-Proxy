// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处
	FileKey     string // 单文件模式（auth.json）下，该账号在顶层 map 中的键；空 = 整文件模式

	// Provider 上游供应商标识："codebuddy"(默认) / "trae" / "qoder"。
	// 空字符串在运行时按 "codebuddy" 处理（见 ProviderOrDefault）。
	Provider string `json:"provider,omitempty"`

	// Extra 捕获各 provider 专用字段（CodeBuddy 不使用）。
	// 例如 Trae 的 cloudideToken/appId/appVersion；Qoder 的 machine 指纹/dt 等。
	Extra map[string]string `json:"extra,omitempty"`
}

// ProviderOrDefault 返回有效的 provider 名（空 → "codebuddy"）。
func (a *Auth) ProviderOrDefault() string {
	if a == nil {
		return "codebuddy"
	}
	if p := strings.TrimSpace(a.Provider); p != "" {
		return p
	}
	return "codebuddy"
}

// ExtraVal 取 Extra 中的字段（缺省返回空串）。
func (a *Auth) ExtraVal(key string) string {
	if a == nil || a.Extra == nil {
		return ""
	}
	return a.Extra[key]
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Region 返回账号所属区域："intl"（domain 指向 codebuddy.ai 国际版）或 "cn"（默认）。
// 区域路由的权威判定：upstream 选基址、pool 选号、网关多密钥分层共用此口径。
func (a *Auth) Region() string {
	if a != nil && strings.Contains(strings.ToLower(a.Domain), "codebuddy.ai") {
		return "intl"
	}
	return "cn"
}

// InRegion 报告账号是否属于 want 区域；want 为空 = 不限区域（恒 true）。
func (a *Auth) InRegion(want string) bool {
	return want == "" || a.Region() == want
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
		}
	}
	// provider / extra 取自顶层字段（嵌套形与扁平形通用）。
	if raw, ok := probe["provider"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			a.Provider = strings.TrimSpace(s)
		}
	}
	if raw, ok := probe["extra"]; ok {
		var m map[string]string
		if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
			a.Extra = m
		}
	}

	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// doc 还原嵌套形文档（插件可读），供 SaveAtomic 写回。
func (a *Auth) doc() map[string]any {
	d := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// 透传 provider / extra（provider 专用字段不因刷新丢失）。
	if a.Provider != "" {
		d["provider"] = a.Provider
	}
	if len(a.Extra) > 0 {
		d["extra"] = a.Extra
	}
	return d
}

// SaveAtomic 原子写回凭证。
//   - FileKey == ""：整文件模式，当前 Auth 独占该文件（tmp + rename）。
//   - FileKey != ""：单文件模式（auth.json 顶层 map），仅更新该键对应条目，其余账号不动。
//     多账号并发刷新同一文件由 fileMu 串行化，杜绝写穿。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	if a.FileKey != "" {
		return a.saveIntoFile()
	}
	raw, err := json.MarshalIndent(a.doc(), "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// saveIntoFile 单文件模式：读取整个 auth.json，替换 FileKey 对应条目后整体原子写回。
func (a *Auth) saveIntoFile() error {
	fileMu.Lock()
	defer fileMu.Unlock()
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("auth.json parse_error: %w", err)
	}
	entry, err := json.MarshalIndent(a.doc(), "", "  ")
	if err != nil {
		return err
	}
	top[a.FileKey] = entry
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json / trae*.json / qoder*.json / doubao*.json；
// 解析失败的文件静默跳过（启动日志由调用方统计）。
// 文件名只影响"被扫描到"，provider 由文件内 provider 字段决定，与文件名无关。
func LoadDir(dir string) ([]*Auth, error) {
	patterns := []string{
		filepath.Join(dir, "workbuddy*.json"),
		filepath.Join(dir, "trae*.json"),
		filepath.Join(dir, "qoder*.json"),
		filepath.Join(dir, "doubao*.json"),
		filepath.Join(dir, "deepseek*.json"),
	}
	seen := make(map[string]bool)
	var out []*Auth
	for _, pat := range patterns {
		files, err := filepath.Glob(pat)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if seen[f] {
				continue
			}
			seen[f] = true
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			a, err := Parse(raw)
			if err != nil {
				continue
			}
		a.FilePath = f
		out = append(out, a)
		}
	}
	return out, nil
}

// fileMu 保护单文件模式（auth.json）的并发写回：多个账号同时 refresh 时串行进入，
// 避免彼此覆盖未改动的条目。
var fileMu sync.Mutex

// LoadFile 读取单个 auth.json（顶层 map，键为账号标签，值为账号凭证）。
// 每个值经 Parse 解析；解析失败的条目静默跳过。FileKey 记录其顶层键，
// 供 SaveAtomic 单文件模式写回时定位。
func LoadFile(path string) ([]*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("auth.json parse_error: %w", err)
	}
	seen := make(map[string]bool)
	var out []*Auth
	for key, val := range top {
		if seen[key] {
			continue
		}
		seen[key] = true
		a, err := Parse(val)
		if err != nil {
			continue
		}
		a.FilePath = path
		a.FileKey = key
		out = append(out, a)
	}
	return out, nil
}
