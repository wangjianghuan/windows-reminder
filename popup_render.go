package main

import "math"

// 弹窗的几何计算与像素渲染。
//
// 全部自己算有符号距离场（SDF）再转覆盖率，所以圆角、阴影、圆形图标都是
// 抗锯齿的 —— 这是纯 GDI 的 RoundRect/Ellipse 做不到的。
// 输出是「预乘 alpha」的 BGRA，直接喂给 UpdateLayeredWindow。

type layout struct {
	scale        float64
	w, h         int     // 位图尺寸（含阴影留白）
	cardX, cardY float64 // 卡片左上角在位图中的位置
	cardW, cardH float64 // 卡片实际像素尺寸
	radius       float64

	iconCx, iconCy, iconR float64 // 图标圆心与半径（位图像素坐标）
}

// newLayout 按 DPI 缩放和卡片逻辑高度构造布局。
// cardHLogical 传 0 表示用默认高度；否则是「文字量算出来的高度」。
func newLayout(s, cardHLogical float64) layout {
	m := margin * s
	ch := cardH
	if cardHLogical > 0 {
		ch = cardHLogical
	}
	l := layout{
		scale:  s,
		w:      int(math.Round(cardW*s + 2*m)),
		h:      int(math.Round(ch*s + 2*m)),
		cardX:  m,
		cardY:  m,
		cardW:  cardW * s,
		cardH:  ch * s,
		radius: cardRadius * s,
	}
	l.iconCx = l.cardX + iconCx*s
	l.iconCy = l.cardY + l.cardH/2 // 图标始终垂直居中
	l.iconR = iconR * s
	return l
}

// fitCardHeight 由正文位置和实际高度算出卡片总高，并夹在合理区间内。
// 一行字的卡片要比三行字的矮，否则短文案下面会挂一大片白。
//
// topPad 是正文顶部到卡片顶边的距离：有标题时正文要给标题让位（bodyTop），
// 没标题时正文直接顶上去（titleTop）；minH 同理，没标题的卡片可以更矮一些，
// 否则单行字会孤零零地悬在大卡片上半部分。
// extra 是正文下方额外要留的高度（强制休息的倒计时行），上下限都随之放宽。
func fitCardHeight(topPad, bodyLogical, minH, extra float64) float64 {
	h := topPad + bodyLogical + bodyBottomPad + extra
	if h < minH {
		h = minH
	}
	if h > maxCardH+extra {
		h = maxCardH + extra
	}
	return h
}

// px/py 把「以卡片左上角为原点」的逻辑坐标换算成位图像素坐标
func (l layout) px(v float64) int32 { return int32(math.Round(l.cardX + v*l.scale)) }
func (l layout) py(v float64) int32 { return int32(math.Round(l.cardY + v*l.scale)) }

// render 返回像素缓冲和独立的一份 alpha 通道。
// alpha 单独留一份是因为 GDI 画文字时会把 alpha 字节写坏，画完要回填。
func (l layout) render() (pix []byte, alpha []byte) {
	pix = make([]byte, l.w*l.h*4)
	alpha = make([]byte, l.w*l.h)

	cx := l.cardX + l.cardW/2
	cy := l.cardY + l.cardH/2
	hw, hh := l.cardW/2, l.cardH/2
	offY := shadowOffsetY * l.scale
	blur := shadowBlur * l.scale

	for y := 0; y < l.h; y++ {
		pyc := float64(y) + 0.5
		for x := 0; x < l.w; x++ {
			pxc := float64(x) + 0.5
			idx := y*l.w + x

			// 卡片：圆角矩形的有符号距离 → 覆盖率（自带 1px 抗锯齿）
			d := sdRoundRect(pxc-cx, pyc-cy, hw, hh, l.radius)
			card := coverage(d)

			// 阴影：把卡片整体下移当作阴影形状，向外按 smoothstep 衰减
			sd := sdRoundRect(pxc-cx, pyc-cy-offY, hw, hh, l.radius)
			sh := shadowAlpha * (1 - smoothstep(0, blur, sd))

			if card <= 0 && sh <= 0.002 {
				continue
			}
			a := card + sh*(1-card)
			if a <= 0.002 {
				continue
			}

			// 卡片本体：极轻的纵向渐变，避免整块死白
			t := clamp01((pyc - l.cardY) / l.cardH)
			cr, cg, cb := 1.0, 1.0-0.006*t, 1.0

			// 内描边：贴边 1.4px 混入淡灰，让白卡片在浅色桌面上也有轮廓
			edge := smoothstep(-1.4, -0.1, d) * 0.85 * card
			cr += (borderRGB.r - cr) * edge
			cg += (borderRGB.g - cg) * edge
			cb += (borderRGB.b - cb) * edge

			// 预乘 alpha：卡片不透明、阴影纯黑，所以 RGB 只需乘卡片覆盖率
			pix[idx*4+0] = toByte(cb * card)
			pix[idx*4+1] = toByte(cg * card)
			pix[idx*4+2] = toByte(cr * card)
			pix[idx*4+3] = toByte(a)
			alpha[idx] = toByte(a)
		}
	}

	l.drawIcon(pix)
	return pix, alpha
}

// drawIcon 画左侧的圆形图标：淡蓝底 + 白色暂停符号。
//
// 试过时钟指针（12 点 + 3 点方向），在 32px 下会读成字母 L；
// 10:10 的对称姿态又会读成对勾。暂停符号在这个尺寸下辨识度最高，
// 含义也正好是「停一下」。
func (l layout) drawIcon(pix []byte) {
	ix, iy, r := l.iconCx, l.iconCy, l.iconR

	barW := 3.4 * l.scale  // 一根竖条的宽度
	barH := 13.0 * l.scale // 竖条高度
	gap := 4.8 * l.scale   // 两根之间的间距
	off := gap/2 + barW/2  // 中心到竖条中心的距离

	x0 := clampInt(int(math.Floor(ix-r-2)), 0, l.w)
	x1 := clampInt(int(math.Ceil(ix+r+2)), 0, l.w)
	y0 := clampInt(int(math.Floor(iy-r-2)), 0, l.h)
	y1 := clampInt(int(math.Ceil(iy+r+2)), 0, l.h)

	for y := y0; y < y1; y++ {
		pyc := float64(y) + 0.5
		for x := x0; x < x1; x++ {
			pxc := float64(x) + 0.5
			idx := y*l.w + x

			if c := coverage(math.Hypot(pxc-ix, pyc-iy) - r); c > 0 {
				blendRGB(pix, idx, accentRGB, c)
			}

			// 两根圆头竖条 = 暂停符号
			d1 := sdRoundRect(pxc-(ix-off), pyc-iy, barW/2, barH/2, barW/2)
			d2 := sdRoundRect(pxc-(ix+off), pyc-iy, barW/2, barH/2, barW/2)
			if c := coverage(math.Min(d1, d2)); c > 0 {
				blendRGB(pix, idx, color3{1, 1, 1}, c)
			}
		}
	}
}

// blendRGB 把不透明前景色按覆盖率叠到像素上（只用于卡片内部，alpha 保持 255）
func blendRGB(pix []byte, idx int, c color3, a float64) {
	if a <= 0 {
		return
	}
	inv := 1 - a
	i := idx * 4
	pix[i+0] = toByte(float64(pix[i+0])/255*inv + c.b*a)
	pix[i+1] = toByte(float64(pix[i+1])/255*inv + c.g*a)
	pix[i+2] = toByte(float64(pix[i+2])/255*inv + c.r*a)
}

func toByte(v float64) byte {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return byte(v*255 + 0.5)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func smoothstep(e0, e1, x float64) float64 {
	t := clamp01((x - e0) / (e1 - e0))
	return t * t * (3 - 2*t)
}

// coverage 把有符号距离转成覆盖率：≤ -0.5 完全覆盖，≥ 0.5 完全不覆盖
func coverage(d float64) float64 {
	if d <= -0.5 {
		return 1
	}
	if d >= 0.5 {
		return 0
	}
	return 0.5 - d
}

// sdRoundRect 圆角矩形有符号距离（(px,py) 相对于矩形中心，(hw,hh) 是半宽半高）
func sdRoundRect(px, py, hw, hh, r float64) float64 {
	qx := math.Abs(px) - hw + r
	qy := math.Abs(py) - hh + r
	ox := math.Max(qx, 0)
	oy := math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}
