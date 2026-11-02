// qoder_sse.go 处理 Qoder 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。移植自 qoderwork2api。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// QoderBizError 上游在 HTTP 200 的 SSE 里下发的业务错误，例如：
//
//	data: {"code":"105","message":"Login expired","model":"qmodel_preview"}
//
// 这类帧没有 body 字段，必须显式识别并立刻中断，否则连接会一直被上游挂着
// 直到 HTTP 超时（实测会挂满 120s，客户端只看到空响应）。
type QoderBizError struct {
	Code    string
	Message string
}

func (e *QoderBizError) Error() string {
	if e.Code == "" {
		return "qoder upstream error: " + e.Message
	}
	return "qoder upstream code=" + e.Code + ": " + e.Message
}

// qoderCodeOK 判断业务 code 是否代表成功（缺省/0/200 均视为成功）。
func qoderCodeOK(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case float64:
		return t == 0 || t == 200
	case string:
		s := strings.TrimSpace(t)
		return s == "" || s == "0" || s == "200"
	case bool:
		return t
	}
	return true
}

// ParseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - body == "[DONE]" → 正常结束
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
//   - 顶层帧带非成功 code（无 body）→ 返回 *QoderBizError，立即中断
func ParseNestedSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var top map[string]any
			if json.Unmarshal([]byte(payload), &top) == nil {
				if body, _ := top["body"].(string); body != "" {
					if body == "[DONE]" {
						return nil
					}
					var chunk map[string]any
					if json.Unmarshal([]byte(body), &chunk) == nil {
						if err := onChunk(chunk); err != nil {
							return err
						}
					}
				} else if code, has := top["code"]; has && !qoderCodeOK(code) {
					// 顶层业务错误帧：立即中断，不再等上游关闭连接。
					msg, _ := top["message"].(string)
					if msg == "" {
						msg, _ = top["msg"].(string)
					}
					return &QoderBizError{Code: fmt.Sprint(code), Message: msg}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// qoderAggregate 聚合完整嵌套 SSE 为单个 OpenAI chat.completion 响应。
// model 用于覆盖响应中的 model 字段（上游恒为 "auto"，客户端要看到自己的模型名）。
func qoderAggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				if r2, ok := delta["role"].(string); ok && r2 != "" {
					role = r2
				}
				if txt, ok := delta["content"].(string); ok {
					content.WriteString(txt)
				}
				if rc, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(rc)
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// qoderStreamTo 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端（带 usage 终帧）。
func qoderStreamTo(w http.ResponseWriter, r io.Reader, model string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := float64(time.Now().Unix())
	var lastUsage map[string]any
	writeChunk := func(delta map[string]any, finish string, usage map[string]any) error {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		} else {
			choice["finish_reason"] = nil
		}
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": int64(created),
			"model":   model,
			"choices": []any{choice},
		}
		if usage != nil {
			chunk["usage"] = usage
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	err := ParseNestedSSE(r, func(chunk map[string]any) error {
		chunk["model"] = model
		normalizeDeltaKeys(chunk)
		// 提取 usage（嵌套 chunk 偶尔把 usage 放在顶层）
		if u, ok := chunk["usage"].(map[string]any); ok {
			lastUsage = u
		}
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 终帧：补 finish（usage 已在最后数据帧或此处带出），保证恰好一个 [DONE]
	_ = writeChunk(map[string]any{}, "stop", lastUsage)
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// normalizeDeltaKeys 删除空的 content/reasoning_content 键（就地修改 chunk）。
func normalizeDeltaKeys(chunk map[string]any) {
	chs, _ := chunk["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		d, _ := c["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if v, ok := d["content"].(string); ok && v == "" {
			delete(d, "content")
		}
		if v, ok := d["reasoning_content"].(string); ok && v == "" {
			delete(d, "reasoning_content")
		}
	}
}
