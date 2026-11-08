// Package prompt 提供网关自有系统提示词（内置默认 + 文件覆盖 + 降级中性提示词）。
//
// 出站前替换客户端 system/developer 消息，避免其固定模板句被上游内容审核误杀。
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded 降级提示词：指纹误报重试时用最小中性提示词，刻意极简。
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load 按 mode 与 file 加载系统提示词文本：file 为空返回内置默认，否则读文件。
func Load(mode, file string) (string, error) {
	if file == "" { return defaultPrompt, nil }
	raw, err := os.ReadFile(file)
	if err != nil { return "", fmt.Errorf("prompt file %s: %w", file, err) }
	return string(raw), nil
}

// Rewrite 用 systemPrompt 替换请求体中所有 system/developer 消息。
// 解析失败原样返回：改写位于出站关键路径，不能阻塞转发。
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" { return body }
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil { return body }
	msgs, ok := obj["messages"].([]any)
	if !ok {

		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil { return out }
		return body
	}

	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" { continue }
		kept = append(kept, m)
	}

	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil { return body }
	return out
}

// EnsureSystem 保证 messages 首条是 system：不足则前插 fallback，从不改写既有消息。
// 国际版 codebuddy.ai 强校验首条必须为 system，否则 400 code 11128。
func EnsureSystem(body []byte, fallback string) []byte {
	if len(body) == 0 || fallback == "" { return body }
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil { return body }
	msgs, ok := obj["messages"].([]any)
	if ok && len(msgs) > 0 {
		if m, ok := msgs[0].(map[string]any); ok {
			if role, _ := m["role"].(string); role == "system" { return body }
		}
	}
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": fallback}},
		msgs...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil { return body }
	return out
}
