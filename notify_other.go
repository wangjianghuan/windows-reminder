//go:build !windows

package main

import "fmt"

// 非 Windows 平台降级成终端输出，保证项目在 macOS / Linux 上也能编译运行。
func notify(title, msg, _ string, _ int) error {
	fmt.Printf("\a[提醒] %s — %s\n", title, msg)
	return nil
}
