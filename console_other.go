//go:build !windows

package main

// 非 Windows 平台在终端里运行，窗口不会自己消失，不需要停住。
func holdIfDoubleClicked() {}

// 没有隐藏控制台的概念，留空实现让 main 里的调用保持平台无关。
func hideConsole() {}

func showConsole() {}
