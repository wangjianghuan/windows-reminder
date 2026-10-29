package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// at 造一个本地时间的辅助函数，避免测试里到处写 time.Date。
func at(y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, time.Local)
}

// 2026-10-27 是周二。
func testConfig() Config {
	return Config{
		Enabled:      true,
		WeekdaysOnly: true,
		Title:        "该休息了",
		Message:      "默认文案",
		Position:     PosCenter,
		Windows: []WorkWindow{
			{Start: "09:00", End: "11:00", Interval: 60, Message: "上午场"},
			{Start: "13:30", End: "17:30", Interval: 60, Message: "下午场"},
			{Start: "18:00", End: "20:30", Interval: 60}, // 没写 message，应回落到默认
		},
	}
}

func TestNextReminderPicksOwningWindow(t *testing.T) {
	cfg := testConfig()

	cases := []struct {
		name    string
		now     time.Time
		want    time.Time
		wantMsg string
	}{
		{"早上 8 点 → 第一个时段的起点", at(2026, 10, 27, 8, 0), at(2026, 10, 27, 9, 0), "上午场"},
		{"10:30 → 上午场下一个整点", at(2026, 10, 27, 10, 30), at(2026, 10, 27, 11, 0), "上午场"},
		{"午休 12:00 → 下午场第一个点", at(2026, 10, 27, 12, 0), at(2026, 10, 27, 13, 30), "下午场"},
		{"17:31 → 第三段（无自定义文案）", at(2026, 10, 27, 17, 31), at(2026, 10, 27, 18, 0), "默认文案"},
		{"20:31 → 明天 9 点", at(2026, 10, 27, 20, 31), at(2026, 10, 28, 9, 0), "上午场"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, win, ok := NextReminder(cfg, c.now)
			if !ok {
				t.Fatalf("期望有提醒，实际没有")
			}
			if !got.Equal(c.want) {
				t.Fatalf("时刻 = %s，期望 %s", got.Format("01-02 15:04"), c.want.Format("01-02 15:04"))
			}
			if msg := win.ResolveMessage(cfg.Message); msg != c.wantMsg {
				t.Fatalf("文案 = %q，期望 %q", msg, c.wantMsg)
			}
		})
	}
}

func TestWeekdaysOnlySkipsWeekend(t *testing.T) {
	cfg := testConfig()
	// 2026-10-31 是周六，下一个提醒应是周一 2026-11-02 09:00
	got, _, ok := NextReminder(cfg, at(2026, 10, 31, 8, 0))
	if !ok {
		t.Fatal("期望有提醒，实际没有")
	}
	want := at(2026, 11, 2, 9, 0)
	if !got.Equal(want) {
		t.Fatalf("时刻 = %s，期望 %s", got.Format("01-02 15:04"), want.Format("01-02 15:04"))
	}
}

func TestDisabledAndEmpty(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	if _, _, ok := NextReminder(cfg, time.Now()); ok {
		t.Fatal("总开关关闭时不应该有提醒")
	}

	cfg = testConfig()
	cfg.Windows = nil
	if _, _, ok := NextReminder(cfg, time.Now()); ok {
		t.Fatal("没有时段时不应该有提醒")
	}
}

func TestTodayScheduleMergedAndSorted(t *testing.T) {
	cfg := testConfig()
	pts := TodaySchedule(cfg, at(2026, 10, 27, 7, 0))

	// 三段分别 3 + 5 + 3 个点
	if len(pts) != 11 {
		t.Fatalf("今日提醒点 = %d 个，期望 11 个", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if pts[i].At.Before(pts[i-1].At) {
			t.Fatalf("第 %d 个点时间倒序了：%s < %s",
				i, pts[i].At.Format("15:04"), pts[i-1].At.Format("15:04"))
		}
	}
	// 每个点都要能找到产生它的时段文案
	for _, p := range pts {
		if msg := p.Win.ResolveMessage(cfg.Message); msg == "" {
			t.Fatalf("%s 这个点没有对应文案", p.At.Format("15:04"))
		}
	}
}

func TestResolveFallsBack(t *testing.T) {
	w := WorkWindow{Message: "   ", Title: ""}
	if got := w.ResolveMessage("兜底"); got != "兜底" {
		t.Fatalf("空白文案应回落，实际 %q", got)
	}
	if got := w.ResolveTitle("兜底标题"); got != "兜底标题" {
		t.Fatalf("空标题应回落，实际 %q", got)
	}
	w = WorkWindow{Message: "  自定义  ", Title: " 专属 "}
	if got := w.ResolveMessage("兜底"); got != "自定义" {
		t.Fatalf("文案应被 trim，实际 %q", got)
	}
	if got := w.ResolveTitle("兜底"); got != "专属" {
		t.Fatalf("标题应被 trim，实际 %q", got)
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c := testConfig()
		c.Normalize()
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("默认配置不应报错：%v", err)
	}

	bad := base()
	bad.Windows[0].Message = strings.Repeat("字", maxContentRunes+1)
	if err := bad.Validate(); err == nil {
		t.Fatal("超长文案应被拒绝")
	}

	ok := base()
	ok.Windows[0].Message = strings.Repeat("字", maxContentRunes)
	if err := ok.Validate(); err != nil {
		t.Fatalf("正好 100 字应通过：%v", err)
	}

	badPos := base()
	badPos.Position = "middle-center"
	if err := badPos.Validate(); err == nil {
		t.Fatal("非法位置应被拒绝")
	}
}

func TestNormalizeLegacyConfig(t *testing.T) {
	// 老配置文件没有 position 字段，应自动落到「正中」而不是报错
	c := Config{Title: "x", Message: "y"}
	c.Normalize()
	if c.Position != PosCenter {
		t.Fatalf("缺省位置 = %q，期望 %q", c.Position, PosCenter)
	}
	if c.Windows == nil {
		t.Fatal("Windows 应被初始化为空切片而非 nil")
	}
	// closeAfter 也是后加的字段，老配置解出来是 0，应补成默认 6 秒
	if c.CloseAfter != defaultCloseAfter {
		t.Fatalf("缺省停留秒数 = %d，期望 %d", c.CloseAfter, defaultCloseAfter)
	}
}

// 弹窗停留秒数：0 / 负数补默认值，超上限报错，正常值原样保留。
func TestCloseAfter(t *testing.T) {
	for _, in := range []int{0, -3} {
		c := Config{CloseAfter: in}
		c.Normalize()
		if c.CloseAfter != defaultCloseAfter {
			t.Fatalf("Normalize(%d) = %d，期望 %d", in, c.CloseAfter, defaultCloseAfter)
		}
	}

	c := Config{Title: "t", Message: "m", Position: PosCenter, Windows: []WorkWindow{}}
	for _, sec := range []int{1, 6, 30, maxCloseAfter} {
		c.CloseAfter = sec
		if err := c.Validate(); err != nil {
			t.Fatalf("停留 %d 秒不该报错：%v", sec, err)
		}
	}
	c.CloseAfter = maxCloseAfter + 1
	if err := c.Validate(); err == nil {
		t.Fatalf("停留 %d 秒应被拒绝", c.CloseAfter)
	}
}

// 强制休息是纯布尔开关：老配置没有这个字段时必须是关闭的，
// 打开后写盘再读回也要原样保住（不能和 closeAfter 互相干扰）。
func TestForceRest(t *testing.T) {
	legacy := Config{Title: "t", Message: "m"}
	legacy.Normalize()
	if legacy.ForceRest {
		t.Fatal("老配置缺 forceRest 字段时应为 false")
	}

	on := Config{
		Title: "t", Message: "m", Position: PosCenter,
		CloseAfter: 60, ForceRest: true, Windows: []WorkWindow{},
	}
	data, err := json.Marshal(on)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if !back.ForceRest {
		t.Fatalf("强制休息没有写进 JSON：%s", data)
	}
	if back.CloseAfter != 60 {
		t.Fatalf("强制休息影响了停留时间：%d，期望 60", back.CloseAfter)
	}
}

// 间隔填 0 表示「仅一次」：只在时段开始那一刻提醒，之后到结束都不再提醒。
func TestOnceWindow(t *testing.T) {
	w := WorkWindow{Start: "10:00", End: "18:00", Interval: 0}
	day := at(2026, 10, 27, 0, 0)

	pts := w.PointsOn(day)
	if len(pts) != 1 {
		t.Fatalf("仅一次应只有 1 个时刻，实际 %d 个", len(pts))
	}
	if want := at(2026, 10, 27, 10, 0); !pts[0].Equal(want) {
		t.Fatalf("时刻 = %s，期望 %s", pts[0].Format("15:04"), want.Format("15:04"))
	}

	cfg := testConfig()
	cfg.Windows = []WorkWindow{w}

	got, _, ok := NextReminder(cfg, at(2026, 10, 27, 9, 0))
	if !ok || !got.Equal(at(2026, 10, 27, 10, 0)) {
		t.Fatalf("9:00 时下一个提醒应是当天 10:00，实际 %s", got.Format("01-02 15:04"))
	}

	// 10:01 已经错过今天那一次，应该跳到明天同一时刻，而不是在时段内继续提醒
	got, _, ok = NextReminder(cfg, at(2026, 10, 27, 10, 1))
	if !ok || !got.Equal(at(2026, 10, 28, 10, 0)) {
		t.Fatalf("错过之后应跳到次日 10:00，实际 %s", got.Format("01-02 15:04"))
	}

	if n := len(TodaySchedule(cfg, day)); n != 1 {
		t.Fatalf("今天的提醒点 = %d 个，期望 1 个", n)
	}
}

// 标题留空是合法配置，语义是「弹窗里不显示标题」，不能偷偷塞回默认标题。
func TestEmptyTitleIsAllowed(t *testing.T) {
	c := Config{Message: "只有一句话"}
	c.Normalize()
	if c.Title != "" {
		t.Fatalf("标题留空不应被填上默认值，实际 %q", c.Title)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("空标题应当合法：%v", err)
	}
	if got := (WorkWindow{}).ResolveTitle(c.Title); got != "" {
		t.Fatalf("时段和全局都没写标题时应解析为空，实际 %q", got)
	}

	// 时段没写标题时仍然回落到全局标题
	if got := (WorkWindow{}).ResolveTitle("该休息了"); got != "该休息了" {
		t.Fatalf("应回落到全局标题，实际 %q", got)
	}

	long := Config{Message: "正文", Title: strings.Repeat("标", maxTitleRunes+1)}
	if err := long.Validate(); err == nil {
		t.Fatal("超长标题应被拒绝")
	}

	negative := Config{Message: "正文", Windows: []WorkWindow{{Start: "09:00", End: "10:00", Interval: -1}}}
	negative.Normalize()
	if err := negative.Validate(); err == nil {
		t.Fatal("负数间隔应被拒绝")
	}
}
