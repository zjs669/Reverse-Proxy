// page.go 内嵌前端页面。
package admin

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// page 返回管理台单页。所有 #/xxx 路由都是前端处理的，这里只吐同一个 HTML。
func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 管理台页面不缓存，避免改完代码浏览器还跑旧版
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}
