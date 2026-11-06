// deepseek_file.go DeepSeek 网页版「带文件对话」：图片走「先上传拿 file id，
// 再放进 completion 的 ref_file_ids」两步。
//
// 协议要点（2026-11-03 浏览器实测）：
//   POST /api/v0/file/upload_file   multipart/form-data，字段名固定 "file"
//       成功返回 {"id":"file-<uuid>","status":"PENDING","is_image":bool,
//                "model_kind":"VISION","audit_result":null|"unknown"|"pass"}
//       注意 model_kind 恒为 VISION，真正的类型判定是 is_image。
//   GET  /api/v0/file/fetch_files?file_ids=<id>
//       轮询用，直到 status=SUCCESS：
//         - 图片：还要 audit_result=pass（否则引用返回 biz_code=9 invalid ref file id）
//         - 其他：audit_result=null，SUCCESS 即可
//   completion 里 ref_file_ids: ["file-<uuid>"]，且 search_enabled 必须为 false
//   （上游有 SEARCH_CANNOT_WITH_FILE，文件与联网搜索互斥）。
//
// 上传用的 PoW 是独立的：create_pow_challenge 的 target_path 必须是
// "/api/v0/file/upload_file"，与 completion 的 PoW 不是同一个。
package upstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"reverse-proxy/internal/auth"
)

// deepseekUploadPath 文件上传端点（也是上传 PoW 的 target_path）。
const deepseekUploadPath = "/api/v0/file/upload_file"

// deepseekFileFetchPath 文件状态查询端点。
const deepseekFileFetchPath = "/api/v0/file/fetch_files"

// dsFileReadyTimeout 等待文件就绪的总时长。
// 实测图片要 20~40s（PENDING → PARSING → SUCCESS），文本几乎立刻 SUCCESS。
const dsFileReadyTimeout = 60 * time.Second

// dsFilePollInterval 轮询间隔。
const dsFilePollInterval = 1500 * time.Millisecond

// dsMaxInlineImageBytes 单张内联图片的大小上限（解码后）。
// 上游单文件上限 100MB，但内联 base64 会先占满网关内存，这里收在 20MB。
const dsMaxInlineImageBytes = 20 << 20

// dsAttachment 一张待上传的图片。
type dsAttachment struct {
	Name string // 文件名（上游按扩展名判类型，必须带正确后缀）
	MIME string // 内容类型
	Data []byte // 原始字节
}

// dsExtractImages 从 OpenAI messages 里提取最后一条 user 消息的图片。
//
// 支持 OpenAI 规范的 content 数组写法：
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,xxx"}}
//
// 只认 data URL（见 dsParseDataURL 的 SSRF 说明）。
// 返回 nil 表示本轮没有图片。
func dsExtractImages(reqBody map[string]any) ([]dsAttachment, error) {
	msgs, _ := reqBody["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			return nil, nil // 纯文本 user 消息，无图片
		}
		var out []dsAttachment
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := pm["type"].(string); t != "image_url" {
				continue
			}
			img, _ := pm["image_url"].(map[string]any)
			if img == nil {
				continue
			}
			raw, _ := img["url"].(string)
			att, err := dsParseDataURL(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, att)
		}
		return out, nil
	}
	return nil, nil
}

// dsParseDataURL 解析 data:image/png;base64,xxx 形式的图片。
//
// 只支持 data URL：远程 http(s) 图片需要网关先去下载，那会引入 SSRF 面
// （客户端可让网关访问任意内网地址），故明确拒绝并提示改用 base64。
func dsParseDataURL(raw string) (dsAttachment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dsAttachment{}, fmt.Errorf("deepseek: image_url 为空")
	}
	if !strings.HasPrefix(raw, "data:") {
		return dsAttachment{}, fmt.Errorf(
			"deepseek: 只支持 base64 data URL（data:image/png;base64,...），"+
				"不支持远程地址 %q——请让客户端内联 base64", truncate(raw, 60))
	}
	comma := strings.Index(raw, ",")
	if comma < 0 {
		return dsAttachment{}, fmt.Errorf("deepseek: data URL 缺少逗号分隔")
	}
	meta := raw[5:comma]
	if !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return dsAttachment{}, fmt.Errorf("deepseek: data URL 必须是 base64 编码（缺 ;base64）")
	}
	mime := strings.TrimSpace(meta[:len(meta)-len(";base64")])
	if mime == "" {
		mime = "application/octet-stream"
	}
	// base64 可能被客户端按行折断，先清掉空白再解
	payload := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, raw[comma+1:])
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(payload) // URL-safe 变体
		if err != nil {
			return dsAttachment{}, fmt.Errorf("deepseek: base64 解码失败: %w", err)
		}
	}
	if len(data) == 0 {
		return dsAttachment{}, fmt.Errorf("deepseek: 图片内容为空")
	}
	if len(data) > dsMaxInlineImageBytes {
		return dsAttachment{}, fmt.Errorf("deepseek: 图片过大 %d MB（上限 %d MB）",
			len(data)>>20, dsMaxInlineImageBytes>>20)
	}
	return dsAttachment{Name: "image" + dsMimeExt(mime), MIME: mime, Data: data}, nil
}

// dsMimeExt 给 MIME 类型配扩展名（上游按扩展名判文件类型）。
func dsMimeExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	}
	return ".png" // 含 image/png 与未知类型兜底
}

// dsUploadFile 上传一个文件，返回 file id（形如 file-<uuid>）。
//
// 上传有独立的 PoW：target_path 必须是 /api/v0/file/upload_file。
func (p *deepseekProvider) dsUploadFile(c *Client, a *auth.Auth, att dsAttachment) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", att.Name)
	if err != nil {
		return "", fmt.Errorf("deepseek: build multipart: %w", err)
	}
	if _, err := fw.Write(att.Data); err != nil {
		return "", fmt.Errorf("deepseek: write multipart: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("deepseek: close multipart: %w", err)
	}

	powHeader, err := p.dsPowHeaderFor(c, a, deepseekUploadPath)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, deepseekBase+deepseekUploadPath, &buf)
	if err != nil {
		return "", err
	}
	p.deepseekHeaders(req, a)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("x-ds-pow-response", powHeader)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", transportErrHint(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	data, err := dsParseEnvelope(resp.StatusCode, raw)
	if err != nil {
		return "", err
	}
	var payload struct {
		ID       string `json:"id"`
		FileName string `json:"file_name"`
		IsImage  bool   `json:"is_image"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("deepseek: parse upload: %w", err)
	}
	if strings.TrimSpace(payload.ID) == "" {
		return "", fmt.Errorf("deepseek: upload_file 未返回 id")
	}
	log.Printf("[deepseek] upload uid=%s file=%s name=%s image=%v size=%d",
		logUID(a.UID), payload.ID, payload.FileName, payload.IsImage, len(att.Data))
	return payload.ID, nil
}

// dsWaitFileReady 轮询到文件可用。
//
// 图片要 audit_result=pass（PENDING/unknown 阶段引用会 biz_code=9），
// 非图片 audit_result 为 null，SUCCESS 即可。
func (p *deepseekProvider) dsWaitFileReady(c *Client, a *auth.Auth, fileID string) error {
	deadline := time.Now().Add(dsFileReadyTimeout)
	for {
		q := url.Values{}
		q.Set("file_ids", fileID)
		req, err := http.NewRequest(http.MethodGet,
			deepseekBase+deepseekFileFetchPath+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		p.deepseekHeaders(req, a)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return transportErrHint(err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode < 400 {
			if data, perr := dsParseEnvelope(resp.StatusCode, raw); perr == nil {
				var payload struct {
					Files []struct {
						Status      string `json:"status"`
						AuditResult string `json:"audit_result"`
						IsImage     bool   `json:"is_image"`
					} `json:"files"`
				}
				if json.Unmarshal(data, &payload) == nil && len(payload.Files) > 0 {
					f := payload.Files[0]
					if f.AuditResult == "reject" {
						return fmt.Errorf("deepseek: 图片未通过审核（audit_result=reject）")
					}
					if f.Status == "SUCCESS" && (!f.IsImage || f.AuditResult == "pass") {
						return nil
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deepseek: 等待文件就绪超时（%s）", dsFileReadyTimeout)
		}
		time.Sleep(dsFilePollInterval)
	}
}

// dsUploadAll 上传全部图片并等可用，返回 file id 列表。
func (p *deepseekProvider) dsUploadAll(c *Client, a *auth.Auth, atts []dsAttachment) ([]string, error) {
	ids := make([]string, 0, len(atts))
	for _, att := range atts {
		id, err := p.dsUploadFile(c, a, att)
		if err != nil {
			return nil, err
		}
		if err := p.dsWaitFileReady(c, a, id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}
