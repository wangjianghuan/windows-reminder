//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"unsafe"
)

var (
	pGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	pGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	// 名字带 Call 后缀，避免和 notify_windows.go 里的 pShowWindow 撞车
	pShowWindowCall = user32.NewProc("ShowWindow")
)

const (
	swHide       = 0
	swShowNormal = 1
)

// consoleHidden 记录控制台是不是被我们藏起来了。
// 只在主 goroutine（启动/退出报错）里读写，不需要加锁。
var consoleHidden bool

// hideConsole 把本进程的控制台窗口藏起来，供 -silent（开机自启）使用。
//
// 只做 SW_HIDE 而不是用 -ldflags="-H windowsgui" 重编：后者根本没有控制台，
// 出错时用户连一句提示都看不到，只能去猜。这样编在需要时还能 showConsole 翻出来。
func hideConsole() {
	h, _, _ := pGetConsoleWindow.Call()
	if h == 0 {
		return
	}
	pShowWindowCall.Call(h, swHide)
	consoleHidden = true
}

// showConsole 把藏起来的控制台再显示出来（启动失败时用）。
func showConsole() {
	if !consoleHidden {
		return
	}
	h, _, _ := pGetConsoleWindow.Call()
	if h == 0 {
		return
	}
	pShowWindowCall.Call(h, swShowNormal)
	consoleHidden = false
}

// holdIfDoubleClicked 在「双击启动」的情况下停住控制台窗口，避免提示信息一闪而过。
//
// 判断依据：GetConsoleProcessList 返回挂在当前控制台上的进程数。
// 资源管理器里双击 exe 时，系统会给它新建一个独占控制台，此时只有自己（n == 1）；
// 从 cmd / PowerShell 里运行时，shell 和本程序共用控制台（n >= 2），不该拦着用户。
//
// 注意：程序若用 -ldflags="-H windowsgui" 编成无控制台版本，这里拿不到控制台，
// 会返回 0，同样不会阻塞。
func holdIfDoubleClicked() {
	var buf [4]uint32
	n, _, _ := pGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n != 1 {
		return
	}
	fmt.Print("\n按回车键关闭此窗口……")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
