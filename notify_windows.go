//go:build windows

package main

import (
	"fmt"
	"log"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// 这个文件用 syscall 直接调 Win32 API 自绘一个弹窗（位置可配置，默认屏幕正中）。
//
// 为什么不用系统 Toast：Toast 依赖通知中心（WinRT + AppX 注册），会被勿扰模式/
// 专注助手/组策略静默拦截，且对未注册 AppID 支持不稳定。
//
// 渲染方案：分层窗口（WS_EX_LAYERED + UpdateLayeredWindow）。
//   - 阴影、圆角、图标全部由本文件逐像素计算覆盖率并写入 32 位位图，
//     所以圆角和阴影是抗锯齿的（纯 GDI 的 RoundRect 是硬锯齿）。
//   - 文字仍交给 GDI 的 DrawTextW 画进同一个位图，因为小字号下 GDI 的
//     ClearType 比任何自绘方案都清晰。
//   - 位图以「预乘 alpha」的 BGRA 交给 UpdateLayeredWindow，窗口外透明区域
//     还能让鼠标直接穿透。
//
// 用到的 API 全部是 Windows 2000 起就存在的（user32/gdi32 + msimg 级），
// 没有 WinRT、没有 XAML、没有 .NET、没有 WebView2，也没有通知中心。
//
// 三个必须遵守的 Win32 约束：
//  1. 创建窗口的线程必须自己跑消息循环 → runtime.LockOSThread
//  2. 窗口过程必须是 syscall.NewCallback 包装的包级变量，否则会被 GC
//  3. 窗口过程里不能 panic，否则整个进程崩 → 加 recover（且必须打日志！）

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW      = user32.NewProc("RegisterClassExW")
	pCreateWindowExW       = user32.NewProc("CreateWindowExW")
	pDefWindowProcW        = user32.NewProc("DefWindowProcW")
	pDestroyWindow         = user32.NewProc("DestroyWindow")
	pShowWindow            = user32.NewProc("ShowWindow")
	pGetMessageW           = user32.NewProc("GetMessageW")
	pPostMessageW          = user32.NewProc("PostMessageW")
	pTranslateMessage      = user32.NewProc("TranslateMessage")
	pDispatchMessageW      = user32.NewProc("DispatchMessageW")
	pPostQuitMessage       = user32.NewProc("PostQuitMessage")
	pSetTimer              = user32.NewProc("SetTimer")
	pKillTimer             = user32.NewProc("KillTimer")
	pSetWindowPos          = user32.NewProc("SetWindowPos")
	pSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	pSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	pUpdateLayeredWindow   = user32.NewProc("UpdateLayeredWindow")
	pGetDC                 = user32.NewProc("GetDC")
	pReleaseDC             = user32.NewProc("ReleaseDC")
	pBeginPaint            = user32.NewProc("BeginPaint")
	pEndPaint              = user32.NewProc("EndPaint")

	// DrawTextW 在 user32.dll，不在 gdi32.dll。
	// 挂错 DLL 时 LazyProc.Call 会 panic "Failed to find ... procedure"，
	// 而这个 panic 会被 wndProc 的 recover 吞掉，表现为"窗口画了一半、后面全没了"。
	pDrawTextW = user32.NewProc("DrawTextW")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	pCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	pCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	pDeleteDC           = gdi32.NewProc("DeleteDC")
	pDeleteObject       = gdi32.NewProc("DeleteObject")
	pSelectObject       = gdi32.NewProc("SelectObject")
	pGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")
	pCreateFontW        = gdi32.NewProc("CreateFontW")
	pSetBkMode          = gdi32.NewProc("SetBkMode")
	pSetTextColor       = gdi32.NewProc("SetTextColor")
)

const (
	wmDestroy    = 0x0002
	wmPaint      = 0x000F
	wmClose      = 0x0010
	wmTimer      = 0x0113
	wmLButtonUp  = 0x0202
	wmEraseBkgnd = 0x0014

	// 程序自己换掉当前卡片时发的消息（WM_APP 起是应用自定义区）。
	// 不能用 WM_CLOSE——强制休息期间那条消息是被无视的，连「试一下」都换不掉卡片。
	wmSupersede = 0x8001

	// 两个定时器：1 = 到点自动关闭；2 = 强制休息期间的心跳（刷倒计时 + 重申置顶）
	timerClose = 1
	timerTick  = 2

	// SetWindowPos 的常用标志：只改 Z 序，不动位置尺寸、不抢焦点
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoActivate = 0x0010

	wsPopup       = 0x80000000
	wsExTopmost   = 0x00000008
	wsExToolWnd   = 0x00000080 // 不出现在任务栏和 Alt+Tab
	wsExNoActive  = 0x08000000 // 不抢焦点，不打断正在打字的人
	wsExLayered   = 0x00080000 // 分层窗口，逐像素 alpha
	swShowNoActiv = 4
	spiGetWorkAre = 0x0030
	logPixelsX    = 88 // GetDeviceCaps 的「每逻辑英寸水平像素」索引

	dtLeft        = 0x0000
	dtRight       = 0x0002
	dtCenterV     = 0x0004
	dtSingleLine  = 0x0020
	dtNoPrefix    = 0x0800
	dtWordBreak   = 0x0010
	dtCalcRect    = 0x0400
	dtEndEllipsis = 0x8000

	translucent = 1 // SetBkMode(TRANSPARENT)

	// ---- 版式，全部是 96dpi 下的逻辑像素，实际尺寸乘 DPI 缩放 ----
	cardW      = 340.0 // 卡片宽
	cardH      = 96.0  // 卡片默认高度（实际按文字量自适应）
	cardRadius = 12.0  // 圆角半径
	cardGap    = 24.0  // 距工作区边缘的边距（仅贴边位置生效）

	shadowOffsetY = 5.0  // 阴影下移量
	shadowBlur    = 14.0 // 阴影扩散半径
	shadowAlpha   = 0.20 // 阴影最深处的 alpha

	margin = 24.0 // 位图四周预留的阴影空间

	iconCx, iconR = 34.0, 16.0 // 左侧图标（垂直位置由卡片高度决定）

	textLeft      = 60.0 // 文字左边
	textRight     = 310.0
	titleTop      = 18.0
	titleBot      = 42.0
	bodyTop       = 46.0
	bodyBottomPad = 14.0  // 正文最后一行到卡片底边的距离
	minCardH      = 76.0  // 卡片高度下限（标题 + 一行正文）
	minCardHNoTtl = 52.0  // 没有标题时的下限：正文一行字，卡片可以矮一截
	maxCardH      = 220.0 // 卡片高度上限（约 8 行，对应 maxContentRunes 字）
	forceHintH    = 22.0  // 强制休息时卡片底部多出的一行倒计时提示
)

var (
	accentRGB = color3{0.169, 0.498, 1.000} // #2B7FFF
	borderRGB = color3{0.878, 0.914, 0.953} // #E0E9F3
)

type color3 struct{ r, g, b float64 }

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type wndMsg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

type smallRect struct {
	left, top, right, bottom int32
}

type paintStruct struct {
	hdc         syscall.Handle
	fErase      int32
	rcPaint     smallRect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type pointT struct{ x, y int32 }
type sizeT struct{ cx, cy int32 }

type blendFunction struct {
	blendOp             byte
	blendFlags          byte
	sourceConstantAlpha byte
	alphaFormat         byte
}

type popupReq struct {
	title, msg  string
	position    string
	closeAfterS int  // 停留秒数，<=0 用默认值
	force       bool // 强制休息：到点之前关不掉
	done        chan error
}

var (
	uiOnce sync.Once
	uiCh   = make(chan popupReq, 8)

	className = utf16Ptr("XiaoBuRestReminderWnd")

	// hwndTopmost 是 SetWindowPos 的 HWND_TOPMOST，也就是 -1。
	hwndTopmost = ^uintptr(0)

	// 只在 UI 线程内读写，无并发问题
	curTitle, curMsg string
	curCountdown     string    // 底部的「还剩 N 秒」，仅在强制休息时非空
	curForce         bool      // 当前卡片是否处于强制休息
	curDeadline      time.Time // 强制休息的结束时刻
	curLayout        layout    // 重绘倒计时要用
	curX, curY       int
	curScale         = 1.0

	// 当前正在显示的窗口句柄（0 表示没有）。跨 goroutine 访问，用原子操作。
	curHwnd uintptr
)

func init() {
	// 必须在创建任何窗口之前声明 DPI 感知，否则高分屏下窗口会糊成一小块。
	// SetProcessDPIAware 从 Vista 起就有，Win8/10/11 都可用。
	_, _, _ = pSetProcessDPIAware.Call()
}

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

func rgb(r, g, b uint32) uintptr {
	return uintptr(r | g<<8 | b<<16)
}

// notify 把请求丢给常驻的 UI 线程，等窗口创建成功就算发送完成（不等用户关闭）。
// position 决定卡片停在工作区的哪个位置，空字符串等价于屏幕正中。
// closeAfterS 是卡片停留几秒后自动关闭（<=0 时用默认值）。
// force 为 true 时进入强制休息：卡片在倒计时走完前点不掉、也不会被盖住。
//
// 如果上一条还在屏幕上，先把它换掉——否则新提醒要排队等上一条自己消失，
// 连点两次「试一下」就会觉得卡住。这里发的是自定义的 wmSupersede，
// 因为 WM_CLOSE 在强制休息期间是被无视的。
func notify(title, msg, position string, closeAfterS int, force bool) error {
	uiOnce.Do(func() { go uiThread() })

	if h := atomic.LoadUintptr(&curHwnd); h != 0 {
		pPostMessageW.Call(h, wmSupersede, 0, 0)
	}

	req := popupReq{
		title: title, msg: msg,
		position: position, closeAfterS: closeAfterS, force: force,
		done: make(chan error, 1),
	}
	select {
	case uiCh <- req:
	default:
		return fmt.Errorf("待显示的提醒过多，已丢弃这一条")
	}
	return <-req.done
}

// uiThread 常驻，串行处理通知请求：一个窗口关掉才显示下一个，
// 避免多条提醒叠在右下角互相遮挡。
func uiThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 用 GetDeviceCaps(LOGPIXELSX) 取 DPI：Win2000 起就有，
	// 比 Win10 才有的 GetDpiForSystem 兼容性好得多。
	screen, _, _ := pGetDC.Call(0)
	if screen != 0 {
		if dpi, _, _ := pGetDeviceCaps.Call(screen, logPixelsX); dpi >= 96 {
			curScale = float64(dpi) / 96.0
		}
		pReleaseDC.Call(0, screen)
	}
	registerClass()

	for req := range uiCh {
		req.done <- showPopup(req.title, req.msg, req.position, req.closeAfterS, req.force)
		messageLoop() // 阻塞到窗口被销毁
	}
}

func registerClass() {
	instance, _, _ := pGetModuleHandleW.Call(0)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         0x0020, // CS_OWNDC
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(instance),
		lpszClassName: className,
	}
	pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
}

func showPopup(title, msg, position string, closeAfterS int, force bool) error {
	// 停留时长由配置决定；<=0 时退回默认值（配置层 Normalize 已经兜过一层，
	// 这里是给 -preview / 内部调用留的保险）。
	if closeAfterS <= 0 {
		closeAfterS = defaultCloseAfter
	}

	curTitle, curMsg = title, msg
	curForce = force
	if force {
		curDeadline = time.Now().Add(time.Duration(closeAfterS) * time.Second)
		curCountdown = countdownText(closeAfterS)
	} else {
		curCountdown = ""
	}

	l := layoutFor(curScale, title, msg, force)

	hwnd, _, err := pCreateWindowExW.Call(
		wsExTopmost|wsExToolWnd|wsExNoActive|wsExLayered,
		uintptr(unsafe.Pointer(className)),
		0, // 无标题
		wsPopup,
		0, 0, uintptr(l.w), uintptr(l.h),
		0, 0, 0, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建提醒窗口失败：%v", err)
	}
	handle := syscall.Handle(hwnd)

	x, y := anchorPos(l, position)
	curLayout, curX, curY = l, x, y

	if perr := paint(handle, l, x, y, true); perr != nil {
		pDestroyWindow.Call(hwnd)
		return perr
	}

	pSetTimer.Call(hwnd, timerClose, uintptr(closeAfterS)*1000, 0)
	if force {
		// 500ms 一跳而不是 1000ms：心跳和关闭定时器各走各的，相位不固定，
		// 1 秒一跳会出现「刚跳到 1 就关了」这种少显示一秒的错觉。
		pSetTimer.Call(hwnd, timerTick, 500, 0)
	}
	atomic.StoreUintptr(&curHwnd, uintptr(hwnd))
	pShowWindow.Call(hwnd, swShowNoActiv) // 不激活，不抢焦点
	return nil
}

// countdownText 是强制休息卡片底部那行提示。写清楚「还剩几秒」，
// 用户才知道这不是卡住了，而是真的关不掉。
func countdownText(sec int) string {
	if sec < 0 {
		sec = 0
	}
	return fmt.Sprintf("强制休息 · 还剩 %d 秒", sec)
}

// refreshForce 是强制休息期间的心跳：刷新倒计时，并把窗口重新抬到最上层。
//
// 为什么要反复抬：WS_EX_TOPMOST 只保证在「非置顶窗口」之上，同样标了置顶的窗口
// （某些悬浮球、播放器全屏、输入法工具条）后出现时照样能盖住它。
// 每秒重新 SetWindowPos(HWND_TOPMOST) 会把自己挪回置顶组的第一位，
// 且带 SWP_NOACTIVATE，不会抢走你正在打字的焦点。
func refreshForce(hwnd syscall.Handle) {
	if !curForce {
		return
	}
	left := int(math.Ceil(time.Until(curDeadline).Seconds()))
	if left < 0 {
		left = 0
	}
	if s := countdownText(left); s != curCountdown {
		curCountdown = s
		if err := paint(hwnd, curLayout, curX, curY, false); err != nil {
			log.Printf("刷新倒计时失败：%v", err)
		}
	}
	pSetWindowPos.Call(uintptr(hwnd), hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
}

// anchorPos 按配置的锚点算出位图左上角的屏幕坐标。
//
// 注意位图比卡片四周各大一圈 margin（留给阴影），所以要把「卡片想要的位置」
// 减掉卡片的内部偏移，才是位图的真实左上角。
// 工作区（wa）已经排除了任务栏，所以贴底时不会压到任务栏上。
func anchorPos(l layout, position string) (int, int) {
	var wa smallRect
	pSystemParametersInfoW.Call(spiGetWorkAre, 0, uintptr(unsafe.Pointer(&wa)), 0)

	workL := float64(wa.left)
	workT := float64(wa.top)
	workW := float64(wa.right - wa.left)
	workH := float64(wa.bottom - wa.top)

	gap := cardGap * curScale
	cw, ch := l.cardW, l.cardH

	centerL := workL + (workW-cw)/2
	centerT := workT + (workH-ch)/2

	// 卡片左上角的目标位置（屏幕物理像素）
	var left, top float64
	switch position {
	case PosTopLeft:
		left, top = workL+gap, workT+gap
	case PosTopCenter:
		left, top = centerL, workT+gap
	case PosTopRight:
		left, top = workL+workW-cw-gap, workT+gap
	case PosMiddleLeft:
		left, top = workL+gap, centerT
	case PosMiddleRight:
		left, top = workL+workW-cw-gap, centerT
	case PosBottomLeft:
		left, top = workL+gap, workT+workH-ch-gap
	case PosBottomCenter:
		left, top = centerL, workT+workH-ch-gap
	case PosBottomRight:
		left, top = workL+workW-cw-gap, workT+workH-ch-gap
	default: // PosCenter，以及任何没识别的值都当居中
		left, top = centerL, centerT
	}

	return int(math.Round(left - l.cardX)), int(math.Round(top - l.cardY))
}

// paint 把整张卡片渲染进 32 位 DIB，再用 UpdateLayeredWindow 贴到屏幕上。
// verbose 只控制要不要打那行尺寸日志：首帧打，逐秒刷新倒计时时不打。
func paint(hwnd syscall.Handle, l layout, x, y int, verbose bool) error {
	screen, _, _ := pGetDC.Call(0)
	if screen == 0 {
		return fmt.Errorf("获取屏幕 DC 失败")
	}
	defer pReleaseDC.Call(0, screen)

	mem, _, _ := pCreateCompatibleDC.Call(screen)
	if mem == 0 {
		return fmt.Errorf("创建内存 DC 失败")
	}
	defer pDeleteDC.Call(mem)

	bi := bitmapInfoHeader{
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       int32(l.w),
		biHeight:      -int32(l.h), // 负数 = 自上而下，避免每次翻转行
		biPlanes:      1,
		biBitCount:    32,
		biCompression: 0, // BI_RGB
	}
	var bits unsafe.Pointer
	hbm, _, _ := pCreateDIBSection.Call(mem, uintptr(unsafe.Pointer(&bi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		return fmt.Errorf("创建位图失败")
	}
	defer pDeleteObject.Call(hbm)

	old, _, _ := pSelectObject.Call(mem, hbm)
	defer pSelectObject.Call(mem, old)

	// 1. 先画背景：阴影 + 圆角卡片 + 描边 + 图标（全部自算抗锯齿）
	pix, alpha := l.render()
	dst := unsafe.Slice((*byte)(bits), l.w*l.h*4)
	copy(dst, pix)

	// 2. 文字交给 GDI（小字号下 ClearType 最清晰）
	th, bh := drawTexts(mem, l, curTitle, curMsg, curCountdown)
	if verbose {
		// 每秒刷倒计时时不再打这行，否则强制休息 5 分钟能刷 300 行日志
		log.Printf("[draw] 位图=%dx%d 卡片=%dx%d 标题高=%d 正文高=%d 缩放=%.2f 强制=%v",
			l.w, l.h, int(l.cardW), int(l.cardH), th, bh, l.scale, curForce)
	}

	// 3. GDI 写像素时不维护 alpha 通道，会把它清成 0，这里按算好的覆盖率回填。
	//    文字只落在不透明的卡片内部，所以整体回填是安全的。
	for i, a := range alpha {
		dst[i*4+3] = a
	}

	dstPt := pointT{int32(x), int32(y)}
	sz := sizeT{int32(l.w), int32(l.h)}
	var srcPt pointT
	blend := blendFunction{blendOp: 0, sourceConstantAlpha: 255, alphaFormat: 1}
	ok, _, uerr := pUpdateLayeredWindow.Call(uintptr(hwnd), screen,
		uintptr(unsafe.Pointer(&dstPt)), uintptr(unsafe.Pointer(&sz)),
		mem, uintptr(unsafe.Pointer(&srcPt)), 0,
		uintptr(unsafe.Pointer(&blend)), 2) // ULW_ALPHA
	if ok == 0 {
		return fmt.Errorf("绘制弹窗失败：%v", uerr)
	}
	return nil
}

func drawTexts(hdc uintptr, l layout, title, body, hint string) (int32, int32) {
	pSetBkMode.Call(hdc, translucent)

	// 标题：没填就整块跳过，正文会顶上来到 titleTop 的位置（见 layoutFor）
	var th int32
	if strings.TrimSpace(title) != "" {
		titleFont := makeFont(-15, 600, l.scale)
		old, _, _ := pSelectObject.Call(hdc, titleFont)
		pSetTextColor.Call(hdc, rgb(0x11, 0x18, 0x27))
		r := smallRect{
			left:   l.px(textLeft),
			top:    l.py(titleTop),
			right:  l.px(textRight),
			bottom: l.py(titleBot),
		}
		th, _ = drawText(hdc, title, &r, dtLeft|dtCenterV|dtSingleLine|dtNoPrefix|dtEndEllipsis)
		pSelectObject.Call(hdc, old)
		pDeleteObject.Call(titleFont)
	}

	// 正文（自动换行）。强制休息时底部要留出倒计时那一行。
	bodyBottom := l.cardH/l.scale - bodyBottomPad
	if hint != "" {
		bodyBottom -= forceHintH
	}
	bodyFont := makeFont(-13, 400, l.scale)
	old, _, _ := pSelectObject.Call(hdc, bodyFont)
	pSetTextColor.Call(hdc, rgb(0x6B, 0x72, 0x80))
	r := smallRect{
		left:   l.px(textLeft),
		top:    l.py(bodyTopOf(title)),
		right:  l.px(textRight),
		bottom: l.py(bodyBottom),
	}
	bh, _ := drawText(hdc, body, &r, dtLeft|dtWordBreak|dtNoPrefix)
	pSelectObject.Call(hdc, old)
	pDeleteObject.Call(bodyFont)

	// 底部倒计时：右对齐的浅灰小字。只画字，不加底色——
	// 卡片本身已经很干净，多一块色块反而像出错提示。
	if hint != "" {
		hintFont := makeFont(-11, 400, l.scale)
		old, _, _ := pSelectObject.Call(hdc, hintFont)
		pSetTextColor.Call(hdc, rgb(0x93, 0x9C, 0xA9))
		hr := smallRect{
			left:   l.px(textLeft),
			top:    l.py(l.cardH/l.scale - forceHintH + 2),
			right:  l.px(textRight),
			bottom: l.py(l.cardH/l.scale - 4),
		}
		drawText(hdc, hint, &hr, dtRight|dtCenterV|dtSingleLine|dtNoPrefix)
		pSelectObject.Call(hdc, old)
		pDeleteObject.Call(hintFont)
	}

	return th, bh
}

// bodyTopOf 返回正文的顶部位置：有标题时让位给标题，没标题时直接顶上去。
func bodyTopOf(title string) float64 {
	if strings.TrimSpace(title) == "" {
		return titleTop
	}
	return bodyTop
}

// layoutFor 先量出正文实际占多高，再决定卡片高度：
// 一行字的卡片矮一点，长文案才变高，避免短文案下面挂一大片白。
// 标题为空时不再预留标题那一行，卡片整体会更矮更紧凑。
// force 为 true 时底部多留一行倒计时，卡片相应长高 forceHintH。
func layoutFor(scale float64, title, msg string, force bool) layout {
	textWidth := (textRight - textLeft) * scale
	bodyH := float64(measureBody(scale, msg, textWidth)) / scale
	if bodyH <= 0 {
		bodyH = 17 // 量不出来就按一行算
	}
	minH := minCardH
	if strings.TrimSpace(title) == "" {
		minH = minCardHNoTtl
	}
	extra := 0.0
	if force {
		extra = forceHintH
		minH += extra
	}
	return newLayout(scale, fitCardHeight(bodyTopOf(title), bodyH, minH, extra))
}

// measureBody 用 DT_CALCRECT 让 GDI 在给定宽度下算出正文需要多高。
func measureBody(scale float64, msg string, widthPx float64) int32 {
	if msg == "" {
		return 0
	}
	screen, _, _ := pGetDC.Call(0)
	if screen == 0 {
		return 0
	}
	defer pReleaseDC.Call(0, screen)

	mem, _, _ := pCreateCompatibleDC.Call(screen)
	if mem == 0 {
		return 0
	}
	defer pDeleteDC.Call(mem)

	f := makeFont(-13, 400, scale)
	old, _, _ := pSelectObject.Call(mem, f)
	r := smallRect{right: int32(math.Round(widthPx)), bottom: 2000}
	h, _ := drawText(mem, msg, &r, dtLeft|dtWordBreak|dtNoPrefix|dtCalcRect)
	pSelectObject.Call(mem, old)
	pDeleteObject.Call(f)
	if h <= 0 {
		h = r.bottom
	}
	return h
}

func makeFont(size, weight int, scale float64) uintptr {
	face := utf16Ptr("Microsoft YaHei UI")
	f, _, _ := pCreateFontW.Call(
		uintptr(int(math.Round(float64(size)*scale))), 0, 0, 0,
		uintptr(weight), 0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(face)),
	)
	return f
}

func drawText(hdc uintptr, s string, r *smallRect, format uintptr) (int32, error) {
	p, err := syscall.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	h, _, callErr := pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&p[0])),
		uintptr(len(p)-1), uintptr(unsafe.Pointer(r)), format)
	return int32(h), callErr
}

func messageLoop() {
	var m wndMsg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	defer func() {
		// 回调里 panic 会直接崩进程，兜住它。但必须打日志——
		// 静默 recover 会让"画到一半没了"这种 bug 完全无从查起。
		if r := recover(); r != nil {
			log.Printf("[wndProc] msg=0x%X 处理时 panic：%v", msg, r)
			pDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		}
	}()

	switch msg {
	case wmPaint:
		// 内容由 UpdateLayeredWindow 提供，这里只需要把无效区域标记为已处理
		var ps paintStruct
		pBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		pEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&ps)))
		return 0

	case wmEraseBkgnd:
		return 1 // 背景自己画，避免闪烁

	case wmTimer:
		switch wParam {
		case timerTick:
			// 强制休息的心跳：刷倒计时 + 重申置顶
			refreshForce(hwnd)
		default:
			pKillTimer.Call(uintptr(hwnd), timerClose)
			pKillTimer.Call(uintptr(hwnd), timerTick)
			pDestroyWindow.Call(uintptr(hwnd))
		}
		return 0

	case wmLButtonUp:
		// 强制休息期间点不掉：只能等倒计时走完。
		if curForce {
			return 0
		}
		pDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmClose:
		// 同上。这里包括用户从任务管理器之类的地方发来的关闭请求；
		// 程序自己要换卡片走的是 wmSupersede，不经过这条分支。
		if curForce {
			return 0
		}
		pDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmSupersede:
		pDestroyWindow.Call(uintptr(hwnd))
		return 0

	case wmDestroy:
		atomic.StoreUintptr(&curHwnd, 0)
		curForce, curCountdown = false, ""
		pPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}
