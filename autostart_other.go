//go:build !windows

package main

import "fmt"

// 非 Windows 平台没有注册表 Run 项这套东西。开启时明确报错，
// 而不是假装成功——否则页面上开关会一直显示「开」，实际什么都没发生。
func applyAutoStart(enable bool) error {
	if enable {
		return fmt.Errorf("当前系统不支持开机自动启动（该功能仅 Windows 可用）")
	}
	return nil
}
