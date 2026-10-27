package main

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 触发窗口：如果程序因为休眠/关闭错过了某个提醒点，
// 只要还没超过这么久，就补发一次；超过则直接跳过，避免唤醒后弹一堆通知。
const catchUpWindow = 10 * time.Minute

func parseHHMM(s string) (hour, min int, err error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("时间格式应为 HH:MM")
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("时间格式应为 HH:MM")
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("时分超出有效范围")
	}
	return h, m, nil
}

// PointsOn 计算某一天落在该时段内的所有提醒时刻（含起点和终点）。
// Interval == 0 表示「仅一次」，此时只有起点这一个时刻。
func (w WorkWindow) PointsOn(day time.Time) []time.Time {
	sh, sm, err1 := parseHHMM(w.Start)
	eh, em, err2 := parseHHMM(w.End)
	if err1 != nil || err2 != nil {
		return nil
	}

	loc := day.Location()
	start := time.Date(day.Year(), day.Month(), day.Day(), sh, sm, 0, 0, loc)
	if w.Once() {
		return []time.Time{start}
	}
	end := time.Date(day.Year(), day.Month(), day.Day(), eh, em, 0, 0, loc)
	step := time.Duration(w.Interval) * time.Minute

	var out []time.Time
	for t := start; !t.After(end); t = t.Add(step) {
		out = append(out, t)
		if len(out) > 2000 { // 防御：间隔极小时不至于无限循环
			break
		}
	}
	return out
}

// Point 是一个具体的提醒时刻，连同产生它的时段一起带上——
// 因为每个时段可以有自己的提示内容，光有时间定位不到该弹哪句文案。
type Point struct {
	At  time.Time
	Win WorkWindow
}

// TodaySchedule 返回当天（按当前星期）合并后、升序排列的所有提醒点。
func TodaySchedule(cfg Config, now time.Time) []Point {
	var out []Point
	for _, w := range cfg.Windows {
		for _, p := range w.PointsOn(now) {
			out = append(out, Point{At: p, Win: w})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// TodayPoints 只取时刻，供页面上的时间胶囊使用。
func TodayPoints(cfg Config, now time.Time) []time.Time {
	pts := TodaySchedule(cfg, now)
	out := make([]time.Time, 0, len(pts))
	for _, p := range pts {
		out = append(out, p.At)
	}
	return out
}

// NextReminder 找出严格晚于 now 的下一个提醒点，并返回它所属的时段。
// 今天没有了就往后找，最多看 7 天（覆盖周末 + 空配置的情况）。
func NextReminder(cfg Config, now time.Time) (time.Time, WorkWindow, bool) {
	if !cfg.Enabled || len(cfg.Windows) == 0 {
		return time.Time{}, WorkWindow{}, false
	}

	for d := 0; d <= 7; d++ {
		day := now.AddDate(0, 0, d)
		if cfg.WeekdaysOnly && isWeekend(day) {
			continue
		}

		var best time.Time
		var bestWin WorkWindow
		for _, w := range cfg.Windows {
			for _, p := range w.PointsOn(day) {
				if p.After(now) && (best.IsZero() || p.Before(best)) {
					best, bestWin = p, w
				}
			}
		}
		if !best.IsZero() {
			return best, bestWin, true
		}
	}
	return time.Time{}, WorkWindow{}, false
}

func isWeekend(t time.Time) bool {
	wd := t.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

// InWindow 判断当前时刻是否落在某个时段的 [Start, End] 区间内，
// 供配置页面显示"此刻是否处于提醒时段"。
func InWindow(cfg Config, now time.Time) bool {
	cur := now.Hour()*60 + now.Minute()
	for _, w := range cfg.Windows {
		sh, sm, err1 := parseHHMM(w.Start)
		eh, em, err2 := parseHHMM(w.End)
		if err1 != nil || err2 != nil {
			continue
		}
		if cur >= sh*60+sm && cur <= eh*60+em {
			return true
		}
	}
	return false
}

// RunScheduler 是提醒主循环，应在独立 goroutine 里跑。
// 用 5 秒轮询而不是精确 timer，是为了兼容系统休眠唤醒导致的时间跳变，
// 代价是提醒最多晚 5 秒——对"整点站起来活动一下"完全无所谓。
func RunScheduler(store *Store) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	cfg := store.Get()
	next, win, ok := NextReminder(cfg, time.Now())

	recompute := func(now time.Time) {
		cfg = store.Get()
		next, win, ok = NextReminder(cfg, now)
	}
	describe := func() string {
		if !ok {
			return "当前没有可用的提醒时刻"
		}
		return "下一次提醒：" + next.Format("01-02 15:04") + "（" + previewOf(win.ResolveMessage(cfg.Message), 12) + "）"
	}

	log.Printf("%s", describe())

	for {
		select {
		case <-store.Changed():
			// 配置被页面改动了，立即重算
			recompute(time.Now())
			log.Printf("配置已更新，%s", describe())

		case <-ticker.C:
			now := time.Now()
			if !ok {
				recompute(now)
				continue
			}
			if now.Before(next) {
				continue
			}

			// 到点了。超时太久说明刚从休眠/关机状态恢复，不补发。
			if now.Sub(next) <= catchUpWindow {
				title := win.ResolveTitle(cfg.Title)
				msg := win.ResolveMessage(cfg.Message)
				if err := notify(title, msg, cfg.Position); err != nil {
					log.Printf("通知发送失败：%v", err)
				} else {
					log.Printf("已提醒：%s（计划时刻 %s）", msg, next.Format("15:04"))
				}
			} else {
				log.Printf("跳过过期提醒 %s（延迟 %s，超过补发窗口）",
					next.Format("15:04"), now.Sub(next).Round(time.Minute))
			}

			recompute(now.Add(time.Second))
		}
	}
}

// previewOf 截断长文案用于日志，避免日志被一整段话撑开。
func previewOf(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
