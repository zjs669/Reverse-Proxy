package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 复现：上游 105 帧的真实结构（取自容器日志）
func TestQoderBizErrInBody(t *testing.T) {
	raw := "data:{\"headers\":{\"Content-Type\":[\"application/json\"]},\"body\":\"{\\\"code\\\":\\\"105\\\",\\\"message\\\":\\\"Login expired\\\"}\",\"statusCodeValue\":403,\"statusCode\":\"FORBIDDEN\"}\n"
	var chunks int
	err := ParseNestedSSE(strings.NewReader(raw), func(m map[string]any) error {
		chunks++
		t.Logf("onChunk: %#v", m)
		return nil
	})
	t.Logf("chunks=%d err=%v", chunks, err)
	if err == nil {
		t.Fatalf("期望返回 QoderBizError，实际 nil（错误帧被当成正常 chunk 透传了）")
	}
}

// 正常数据帧不能被误判为错误（回归保护）
func TestQoderNormalFramesNotError(t *testing.T) {
	frames := []string{
		`data:{"headers":{},"body":"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hi\\\"}}]}","statusCodeValue":200,"statusCode":"OK"}`,
		`data:{"body":"[DONE]"}`,
		`data:{"code":"0","message":"ok"}`,
	}
	for _, f := range frames {
		if be := qoderFrameBizError(mustJSON(t, strings.TrimSpace(strings.TrimPrefix(f, "data:")))); be != nil {
			t.Errorf("正常帧被误判为错误: %s -> %v", f, be)
		}
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json %q: %v", s, err)
	}
	return m
}

// peek 路径：从流首部识别 105（不消费字节）
func TestQoderPeekBizError(t *testing.T) {
	// 用程序化构造，避免手写多层转义出错
	inner := `{"code":"105","message":"Login expired"}`
	top := map[string]any{
		"headers":         map[string]any{"Content-Type": []string{"application/json"}},
		"body":            inner,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	}
	raw, _ := json.Marshal(top)
	line := "data:" + string(raw) + "\n"
	be := qoderPeekBizError([]byte(line))
	if be == nil {
		t.Fatalf("peek 未识别出 105，输入=%s", line)
	}
	if be.Code != "105" {
		t.Fatalf("code=%s want 105", be.Code)
	}

	// 正常流不应误判
	normal := map[string]any{
		"body":            `{"choices":[{"delta":{"content":"ok"}}]}`,
		"statusCodeValue": 200,
		"statusCode":      "OK",
	}
	nraw, _ := json.Marshal(normal)
	if be := qoderPeekBizError([]byte("data:" + string(nraw) + "\n")); be != nil {
		t.Fatalf("正常流被误判: %v", be)
	}
}
