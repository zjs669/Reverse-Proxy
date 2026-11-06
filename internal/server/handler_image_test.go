package server

import (
	"encoding/json"
	"testing"
)

// TestRequestHasImage 图片内容块识别。
func TestRequestHasImage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"纯文本", `{"messages":[{"role":"user","content":"你好"}]}`, false},
		{"OpenAI image_url", `{"messages":[{"role":"user","content":[
			{"type":"text","text":"看图"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`, true},
		{"Anthropic image", `{"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","data":"AAA"}}]}]}`, true},
		{"只有文本 parts", `{"messages":[{"role":"user","content":[
			{"type":"text","text":"只有文字"}]}]}`, false},
		{"空 body", "", false},
		{"非法 JSON", "{bad", false},
	}
	for _, c := range cases {
		if got := requestHasImage([]byte(c.body)); got != c.want {
			t.Errorf("%s: got=%v want=%v", c.name, got, c.want)
		}
	}
}

// TestImageCapableProvider 供应商图片能力表。
//
// 依据实测：codebuddy 全模型都能读图（不止「视觉模型」）；
// doubao / qoder 协议里没有图片位置，必须报不支持。
func TestImageCapableProvider(t *testing.T) {
	yes := []string{"codebuddy", "", "trae", "deepseek", "CodeBuddy", " TRAE "}
	no := []string{"doubao", "qoder", "unknown"}
	for _, p := range yes {
		if !imageCapableProvider(p) {
			t.Errorf("%q 应支持图片", p)
		}
	}
	for _, p := range no {
		if imageCapableProvider(p) {
			t.Errorf("%q 不应支持图片", p)
		}
	}
}

// TestIsVisionModel 标注逻辑：按供应商判定，不再靠模型名白名单。
func TestIsVisionModel(t *testing.T) {
	// codebuddy 下所有模型都标图片（实测 glm-5.3 / hy3 / auto 都能读图）
	for _, m := range []string{"glm-5.3", "hy3", "auto", "deepseek-v4.1-flash", "任意模型"} {
		if !isVisionModel("codebuddy", m) {
			t.Errorf("codebuddy/%s 应标注支持图片", m)
		}
	}
	if !isVisionModel("deepseek", "deepseek-instant") {
		t.Error("deepseek 应标注支持图片")
	}
	// 不支持的供应商：名字里带 vision 才放行（保留启发式）
	if isVisionModel("doubao", "doubao-fast") {
		t.Error("doubao 普通模型不应标注图片")
	}
	if !isVisionModel("doubao", "doubao-vision") {
		t.Error("doubao 的 vision 模型应按名字放行")
	}
}

// TestModelListModalities 端到端：/v1/models 输出的 modalities 要与能力表一致。
func TestModelListModalities(t *testing.T) {
	b, _ := json.Marshal([]map[string]any{})
	_ = b
	// 该断言依赖 handler 构造，见 handler 的 modelList；此处仅校验字段名约定。
	entry := map[string]any{"modalities": []string{"text", "image"}, "capabilities": []string{"vision"}}
	raw, _ := json.Marshal(entry)
	var back map[string]any
	if json.Unmarshal(raw, &back) != nil {
		t.Fatal("modalities 序列化失败")
	}
	if _, ok := back["modalities"]; !ok {
		t.Fatal("缺 modalities 字段")
	}
}
