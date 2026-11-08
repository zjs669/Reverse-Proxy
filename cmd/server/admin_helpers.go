// admin_helpers.go 管理台用到的文件级辅助：auth.json 增删、config 密钥读写、日志句柄。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reverse-proxy/internal/admin"
	"reverse-proxy/internal/server"
)

// newSecret 32 字节随机 → 64 位小写 hex，与 gen_account_keys.py 同口径。
func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// authFilePath 从 config.json 的 auth_file 解析绝对路径（相对路径按配置目录展开）。
func authFilePath(cfgPath string) string {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return "./auths/auth.json"
	}
	var c struct {
		AuthFile string `json:"auth_file"`
	}
	_ = json.Unmarshal(raw, &c)
	p := strings.TrimSpace(c.AuthFile)
	if p == "" {
		p = "./auths/auth.json"
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(cfgPath), p)
	}
	return p
}

// removeAuthEntry 从 auth.json 移除某个顶层键（原子替换整文件）。
func removeAuthEntry(path, key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 auth.json 失败: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("auth.json 不是合法对象: %w", err)
	}
	delete(top, key)
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// strMap 从 raw[key] 取出 string→string 映射。
func strMap(raw map[string]any, key string) map[string]string {
	out := map[string]string{}
	if m, ok := raw[key].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// readKeysFromRaw 从 config.json 原始 map 取四类密钥；第二返回值=是否本来就有。
func readKeysFromRaw(raw map[string]any) (admin.KeysDTO, bool, error) {
	k := admin.KeysDTO{APIKeys: strMap(raw, "api_keys"), ProviderKeys: strMap(raw, "provider_keys")}
	if v, ok := raw["api_key"].(string); ok {
		k.APIKey = v
	}
	k.AccountKeys = map[string]map[string]string{}
	if m, ok := raw["account_keys"].(map[string]any); ok {
		for label, val := range m {
			if inner, ok := val.(map[string]any); ok {
				mm := map[string]string{}
				for uid, sv := range inner {
					if str, ok := sv.(string); ok { mm[uid] = str }
				}
				k.AccountKeys[label] = mm
			}
		}
	}
	_, has := raw["account_keys"]
	return k, has, nil
}

// writeKeysToRaw 把四类密钥写回 raw（只动这四个键，其它原样保留）。
func writeKeysToRaw(raw map[string]any, k admin.KeysDTO) error {
	raw["api_key"] = k.APIKey
	raw["api_keys"] = toAnyMap(k.APIKeys)
	raw["provider_keys"] = toAnyMap(k.ProviderKeys)
	ak := map[string]any{}
	for label, m := range k.AccountKeys {
		ak[label] = toAnyMap(m)
	}
	raw["account_keys"] = ak
	return nil
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// logRingHandle 把 server.LogRing 暴露给 admin，同时保存原始指针供 cmd/server 装 io.MultiWriter。
type logRingHandle struct {
	ring *server.LogRing
}

func newLogRingHandle(n int) *logRingHandle {
	return &logRingHandle{ring: server.NewLogRing(n)}
}

// Writer 返回可塞进 io.MultiWriter 的写端。
func (h *logRingHandle) Writer() *server.LogRing { return h.ring }

// Source 返回 admin 需要的只读视图。
func (h *logRingHandle) Source() admin.LogRingSource {
	return ringSource{h.ring}
}

type ringSource struct{ r *server.LogRing }

func (s ringSource) Since(since int64) ([]admin.LogLineDTO, int64) {
	lines, max := s.r.Since(since)
	out := make([]admin.LogLineDTO, 0, len(lines))
	for _, l := range lines {
		out = append(out, admin.LogLineDTO{Seq: l.Seq, Text: l.Text})
	}
	return out, max
}

func (s ringSource) Tail(n int) []admin.LogLineDTO {
	lines := s.r.Tail(n)
	out := make([]admin.LogLineDTO, 0, len(lines))
	for _, l := range lines {
		out = append(out, admin.LogLineDTO{Seq: l.Seq, Text: l.Text})
	}
	return out
}

// usedSecrets 收集配置里所有在用密钥，新增密钥不得撞车（normalize 校验唯一性）。
func usedSecrets(k admin.KeysDTO) map[string]bool {
	used := map[string]bool{k.APIKey: true}
	for k := range k.APIKeys {
		used[k] = true
	}
	for k := range k.ProviderKeys {
		used[k] = true
	}
	for _, m := range k.AccountKeys {
		for _, s := range m {
			used[s] = true
		}
	}
	return used
}

// freshSecret 生成一个不在 used 里的新密钥，8 次尝试仍冲突则报错。
func freshSecret(used map[string]bool) (string, error) {
	for i := 0; i < 8; i++ {
		if s := newSecret(); s != "" && !used[s] {
			return s, nil
		}
	}
	return "", fmt.Errorf("生成唯一密钥失败，请重试")
}

// readKeys 读 config.json 原始 map 并解析出四类密钥。
func (w *cfgWriter) readKeys() (map[string]any, admin.KeysDTO, error) {
	raw, err := w.readRaw()
	if err != nil {
		return nil, admin.KeysDTO{}, err
	}
	k, _, err := readKeysFromRaw(raw)
	return raw, k, err
}
