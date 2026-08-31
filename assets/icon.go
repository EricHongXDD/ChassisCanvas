// Package appassets 提供编译进 CaseScreen 的应用素材。
package appassets

import _ "embed"

// iconPNG 供 Windows 托盘使用；EXE 标题栏和文件图标由发行脚本嵌入 ICO 资源。
//
//go:embed casescreen-icon.png
var iconPNG []byte

// IconPNG 返回只读的 PNG 图标数据。调用方不应修改返回的切片。
func IconPNG() []byte {
	return iconPNG
}
