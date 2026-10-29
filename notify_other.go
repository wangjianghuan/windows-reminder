//go:build !windows

package main

import "fmt"

// 非 Windows 平台降级成终端输出，保证项目在 macOS / Linux 上也能编译运行。
// 没有窗口可画，「强制休息」也就无从谈起，只把意图打到日志里。
func notify(title, msg, _ string, closeAfterS int, force bool) error {
	tag := ""
	if force {
		tag = fmt.Sprintf("（强制休息 %d 秒）", closeAfterS)
	}
	fmt.Printf("\a[提醒] %s — %s%s\n", title, msg, tag)
	return nil
}
