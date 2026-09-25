// Package webui 把前端构建产物 go:embed 进二进制 —— 保留「单文件交付、无外部依赖」的优点。
//
// 实现阶段 Vue3 产物输出到 internal/webui/dist（构建脚本负责），
// 这里只负责暴露成 http.FileSystem。M0 阶段 dist 里是占位页面。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var raw embed.FS

// FS 返回去掉 dist 前缀后的静态文件系统。
func FS() (fs.FS, error) {
	return fs.Sub(raw, "dist")
}

// Handler 直接给出可用于挂载的 http.Handler。
func Handler() (http.Handler, error) {
	sub, err := FS()
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
