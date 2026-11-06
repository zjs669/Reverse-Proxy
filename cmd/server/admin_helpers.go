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

// newSecret 32 字节随机 → 64 位小写 hex，与 gen_account_keys.py 的
// secrets.token_hex(32) 同口径，两边生成的密钥可互换。
func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// authFilePath 从 config.json 的 auth_file 解析 auth.json 绝对路径。
// 相对路径按配置文件所在目录展开（与网关运行时的 cwd 语义一致）。
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

// readKeysFromRaw 从 config.json 原始 map 里取四类密钥。
// 第二个返回值是"是否本来就有"，用于判断要不要新建段。
func readKeysFromRaw(raw map[string]any) (admin.KeysDTO, bool, error) {
	k := admin.KeysDTO{
		APIKeys:      map[string]string{},
		ProviderKeys: map[string]string{},
		AccountKeys:  map[string]map[string]string{},
	}
	if v, ok := raw["api_key"].(string); ok {
		k.APIKey = v
	}
	if m, ok := raw["api_keys"].(map[string]any); ok {
		for key, val := range m {
			if s, ok := val.(string); ok {
				k.APIKeys[key] = s
			}
		}
	}
	if m, ok := raw["provider_keys"].(map[string]any); ok {
		for key, val := range m {
			if s, ok := val.(string); ok {
				k.ProviderKeys[key] = s
			}
		}
	}
	if m, ok := raw["account_keys"].(map[string]any); ok {
		for label, val := range m {
			inner, ok := val.(map[string]any)
			if !ok {
				continue
			}
			mm := map[string]string{}
			for uid, s := range inner {
				if sv, ok := s.(string); ok {
					mm[uid] = sv
				}
			}
			k.AccountKeys[label] = mm
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
