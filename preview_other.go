//go:build !windows

package main

import "fmt"

// 预览图依赖 GDI 画文字，非 Windows 平台没有实现。
func writePreviewPNG(path, title, body string, scale float64) error {
	return fmt.Errorf("预览图生成仅支持 Windows")
}
