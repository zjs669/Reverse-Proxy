package upstream

import (
	"encoding/base64"
	"strings"
	"testing"
)

// 1x1 红色 PNG
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestDsParseDataURL_PNG 标准 data URL 解析成功，扩展名由 MIME 推断。
func TestDsParseDataURL_PNG(t *testing.T) {
	att, err := dsParseDataURL("data:image/png;base64," + tinyPNG)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if att.Name != "image.png" {
		t.Fatalf("扩展名应由 MIME 推断为 .png，got=%q", att.Name)
	}
	if att.MIME != "image/png" {
		t.Fatalf("MIME 错误: %q", att.MIME)
	}
	if len(att.Data) == 0 {
		t.Fatal("解码后内容为空")
	}
}

// TestDsParseDataURL_MimeExt 各图片格式的扩展名映射。
func TestDsParseDataURL_MimeExt(t *testing.T) {
	cases := map[string]string{
		"image/jpeg": "image.jpg",
		"image/jpg":  "image.jpg",
		"image/webp": "image.webp",
		"image/gif":  "image.gif",
		"image/bmp":  "image.bmp",
		"image/png":  "image.png",
		"image/avif": "image.png", // 未知类型兜底 png
	}
	for mime, want := range cases {
		att, err := dsParseDataURL("data:" + mime + ";base64," + tinyPNG)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", mime, err)
			continue
		}
		if att.Name != want {
			t.Errorf("%s: name=%q want %q", mime, att.Name, want)
		}
	}
}

// TestDsParseDataURL_RejectsRemote 远程地址必须拒绝（否则网关变成 SSRF 跳板）。
func TestDsParseDataURL_RejectsRemote(t *testing.T) {
	for _, raw := range []string{
		"https://example.com/a.png",
		"http://127.0.0.1:8080/secret",
		"ftp://x/y.png",
	} {
		if _, err := dsParseDataURL(raw); err == nil {
			t.Errorf("应拒绝远程地址: %s", raw)
		}
	}
}

// TestDsParseDataURL_RejectsBad 非法 data URL 必须报错而不是静默通过。
func TestDsParseDataURL_RejectsBad(t *testing.T) {
	bad := []string{
		"",                              // 空
		"data:image/png,abc",            // 缺 ;base64
		"data:image/png;base64",         // 缺逗号
		"data:image/png;base64,",        // 内容为空
		"data:image/png;base64,!!!!",    // 非法 base64
	}
	for _, raw := range bad {
		if _, err := dsParseDataURL(raw); err == nil {
			t.Errorf("应报错: %q", raw)
		}
	}
}

// TestDsParseDataURL_CleansWhitespace base64 里夹换行/空格也要能解（部分客户端会折行）。
func TestDsParseDataURL_CleansWhitespace(t *testing.T) {
	wrapped := tinyPNG[:20] + "\n" + tinyPNG[20:40] + " " + tinyPNG[40:]
	att, err := dsParseDataURL("data:image/png;base64," + wrapped)
	if err != nil {
		t.Fatalf("带空白的 base64 应能解析: %v", err)
	}
	ref, _ := base64.StdEncoding.DecodeString(tinyPNG)
	if len(att.Data) != len(ref) {
		t.Fatalf("长度不一致: %d vs %d", len(att.Data), len(ref))
	}
}

// TestDsExtractImages_OpenAIShape 从 OpenAI content 数组里提取图片。
func TestDsExtractImages_OpenAIShape(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "前面的话"},
			map[string]any{"role": "assistant", "content": "好的"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "这张图是什么"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64," + tinyPNG,
				}},
			}},
		},
	}
	atts, err := dsExtractImages(body)
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("应提取 1 张图，got=%d", len(atts))
	}
	if !strings.HasSuffix(atts[0].Name, ".png") {
		t.Fatalf("文件名错误: %q", atts[0].Name)
	}
}

// TestDsExtractImages_Multiple 多图。
func TestDsExtractImages_Multiple(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + tinyPNG}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64," + tinyPNG}},
		}}},
	}
	atts, err := dsExtractImages(body)
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("应提取 2 张图，got=%d", len(atts))
	}
}

// TestDsExtractImages_NoImage 纯文本请求不应误判有图。
func TestDsExtractImages_NoImage(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "纯文本"},
		},
	}
	atts, err := dsExtractImages(body)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("不应提取到图片，got=%d", len(atts))
	}
}

// TestDsExtractImages_TextOnlyParts content 是数组但只有 text：不算有图。
func TestDsExtractImages_TextOnlyParts(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "只有文字"},
		}}},
	}
	atts, err := dsExtractImages(body)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(atts) != 0 {
		t.Fatalf("不应提取到图片，got=%d", len(atts))
	}
}

// TestDsParseDataURL_TooLarge 超大图片应被拒（保护网关内存）。
func TestDsParseDataURL_TooLarge(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, dsMaxInlineImageBytes+1))
	if _, err := dsParseDataURL("data:image/png;base64," + big); err == nil {
		t.Fatal("超限图片应被拒绝")
	}
}
