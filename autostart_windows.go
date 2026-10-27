//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// 开机启动走「当前用户」的 Run 项：
//
//	HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Run
//
// 选 HKCU 而不是 HKLM 有两个原因：一是不需要管理员权限（改 HKLM 要 UAC 提权，
// 双击启动的小工具不该要求这个）；二是「开机启动」对个人电脑来说本来就该是
// 当前用户自己的事。
//
// 这里直接调 advapi32 的 RegXxx 系列函数，不引第三方包，也不 shell 出去调 reg.exe
// —— 后者在部分安全策略下会被拦，而且拼接命令行容易出转义问题。

const (
	autoRunSubKey = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoRunValue  = "RestReminder" // 注册表里的值名，用英文避免编码问题

	hkeyCurrentUser = 0x80000001
	keyQueryValue   = 0x0001
	keySetValue     = 0x0002
	regSZ           = 1
	regNoError      = 0
	regFileNotFound = 2 // ERROR_FILE_NOT_FOUND：要删的值本来就不存在
)

var (
	advapi32          = syscall.NewLazyDLL("advapi32.dll")
	pRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	pRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	pRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	pRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

// autoStartCommand 是写进注册表的那条命令行。
// 带上 -silent：开机时不要弹浏览器、不要弹启动卡片、把控制台窗口藏起来，
// 否则每次登录都会莫名其妙跳出一个黑框和一堆页面。
func autoStartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("取不到程序自身路径：%w", err)
	}
	return fmt.Sprintf("\"%s\" -silent", exe), nil
}

// applyAutoStart 打开/关闭开机启动。enable=false 时把值删掉（不存在也算成功）。
func applyAutoStart(enable bool) error {
	sub, err := syscall.UTF16PtrFromString(autoRunSubKey)
	if err != nil {
		return err
	}

	var hKey syscall.Handle
	if r, _, e := pRegOpenKeyExW.Call(
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(sub)),
		0,
		uintptr(keyQueryValue|keySetValue),
		uintptr(unsafe.Pointer(&hKey)),
	); r != regNoError {
		return fmt.Errorf("打不开注册表项 HKCU\\%s（错误码 %d：%v）", autoRunSubKey, int32(r), e)
	}
	defer pRegCloseKey.Call(uintptr(hKey))

	name, err := syscall.UTF16PtrFromString(autoRunValue)
	if err != nil {
		return err
	}

	if !enable {
		r, _, e := pRegDeleteValueW.Call(uintptr(hKey), uintptr(unsafe.Pointer(name)))
		if r != regNoError && r != regFileNotFound {
			return fmt.Errorf("删除开机启动项失败（错误码 %d：%v）", int32(r), e)
		}
		return nil
	}

	cmd, err := autoStartCommand()
	if err != nil {
		return err
	}
	// UTF16FromString 的返回值带结尾 NUL，所以长度按 len*2 传（REG_SZ 要含 NUL）
	data, err := syscall.UTF16FromString(cmd)
	if err != nil {
		return fmt.Errorf("启动命令含有非法字符：%w", err)
	}
	if r, _, e := pRegSetValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(name)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)*2),
	); r != regNoError {
		return fmt.Errorf("写入开机启动项失败（错误码 %d：%v）", int32(r), e)
	}
	return nil
}

// currentAutoStart 读回注册表里的值，供日志核对「现在到底开没开」。
func currentAutoStart() (string, bool) {
	sub, err := syscall.UTF16PtrFromString(autoRunSubKey)
	if err != nil {
		return "", false
	}
	var hKey syscall.Handle
	if r, _, _ := pRegOpenKeyExW.Call(
		uintptr(hkeyCurrentUser),
		uintptr(unsafe.Pointer(sub)),
		0,
		uintptr(keyQueryValue),
		uintptr(unsafe.Pointer(&hKey)),
	); r != regNoError {
		return "", false
	}
	defer pRegCloseKey.Call(uintptr(hKey))

	name, err := syscall.UTF16PtrFromString(autoRunValue)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, 1024)
	size := uint32(len(buf) * 2)
	if r, _, _ := pRegQueryValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(name)),
		0, 0, // lpReserved, lpType（类型不关心）
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	); r != regNoError {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}
