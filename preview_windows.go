//go:build windows

package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"unsafe"
)

// writePreviewPNG 把一张示例卡片渲染成 PNG，全程不创建窗口。
// 调版式时用它：改完 popup_render.go 里的常量，跑一次就能看到结果，
// 不用等整点、也不用截图。
func writePreviewPNG(path, title, body string, scale float64, force bool) error {
	l := layoutFor(scale, title, body, force)
	hint := ""
	if force {
		hint = countdownText(6) // 预览固定按 6 秒画，只看版式对不对
	}

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
		biHeight:      -int32(l.h),
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

	pix, alpha := l.render()
	dst := unsafe.Slice((*byte)(bits), l.w*l.h*4)
	copy(dst, pix)
	drawTexts(mem, l, title, body, hint)

	// 位图里是预乘 alpha 的 BGRA，PNG 要的是直通 alpha 的 RGBA，这里反预乘
	img := image.NewNRGBA(image.Rect(0, 0, l.w, l.h))
	for i := 0; i < l.w*l.h; i++ {
		a := uint32(alpha[i])
		if a == 0 {
			continue
		}
		img.Pix[i*4+0] = unpremul(dst[i*4+2], a) // R
		img.Pix[i*4+1] = unpremul(dst[i*4+1], a) // G
		img.Pix[i*4+2] = unpremul(dst[i*4+0], a) // B
		img.Pix[i*4+3] = uint8(a)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func unpremul(v byte, a uint32) uint8 {
	c := uint32(v) * 255 / a
	if c > 255 {
		c = 255
	}
	return uint8(c)
}
