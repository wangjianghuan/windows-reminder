//go:build windows

package main

import (
	"math"
	"testing"
)

// 强制休息的卡片底部多一行倒计时，卡片必须相应长高一行；
// 否则倒计时会压在正文上，或者被裁到卡片外面。
func TestForceRestLayoutAddsHintRow(t *testing.T) {
	// 正文留空：measureBody("") 直接返回 0，不会碰 GDI，测试不依赖图形环境
	plain := layoutFor(1, "该休息了", "", false)
	forced := layoutFor(1, "该休息了", "", true)

	got := (forced.cardH - plain.cardH) / forced.scale
	if math.Abs(got-forceHintH) > 0.01 {
		t.Fatalf("强制休息卡片应比普通卡片高 %.1f，实际高了 %.2f", forceHintH, got)
	}
	if forced.h != plain.h+int(math.Round(forceHintH)) {
		t.Fatalf("位图高度没跟着长：%d → %d", plain.h, forced.h)
	}
}

// 没标题时卡片本来就矮一截，加上倒计时后仍应比「有标题 + 倒计时」更矮。
func TestForceRestLayoutNoTitle(t *testing.T) {
	withTitle := layoutFor(1, "该休息了", "", true)
	noTitle := layoutFor(1, "", "", true)

	if noTitle.cardH >= withTitle.cardH {
		t.Fatalf("没标题的强制休息卡片应更矮：%v vs %v", noTitle.cardH, withTitle.cardH)
	}
	if noTitle.cardH/1 < minCardHNoTtl+forceHintH-0.01 {
		t.Fatalf("没标题的强制休息卡片太矮，倒计时会被挤掉：%v", noTitle.cardH)
	}
}

// 倒计时文案不能出现负数秒，也不能在 0 秒时还写「还剩 -1 秒」。
func TestCountdownText(t *testing.T) {
	cases := map[int]string{
		30: "强制休息 · 还剩 30 秒",
		1:  "强制休息 · 还剩 1 秒",
		0:  "强制休息 · 还剩 0 秒",
		-5: "强制休息 · 还剩 0 秒",
	}
	for in, want := range cases {
		if got := countdownText(in); got != want {
			t.Fatalf("countdownText(%d) = %q，期望 %q", in, got, want)
		}
	}
}
