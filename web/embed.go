// Package web 内嵌 Web 前端构建产物。
//
// go:embed 不允许引用上级目录，因此本文件必须放在 web/ 下而非 cmd/server/ 下。
// web/dist/index.html 是预置占位页：即使未执行 make web，
// `CGO_ENABLED=0 go build ./cmd/...` 也能通过（C-3 / K13）。
package web

import (
	"embed"
	"io/fs"
)

// DistFS 包含 web/dist 下的全部静态资源（含占位页或真实 Vite 产物）。
//
//go:embed all:dist
var DistFS embed.FS

// Dist 返回以 dist/ 为根的只读子文件系统，供 http.FileServer 与 SPA fallback 使用。
func Dist() (fs.FS, error) {
	return fs.Sub(DistFS, "dist")
}
