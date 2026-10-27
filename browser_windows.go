//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// HideWindow 避免打开浏览器时闪一个黑色控制台窗口
func openBrowser(url string) error {
	cmd := exec.Command("cmd", "/c", "start", "", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}
