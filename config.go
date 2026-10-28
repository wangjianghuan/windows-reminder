package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	defaultPort = 7788

	// 单条提示内容的字数上限。弹窗高度会按文字量自适应，
	// 但不会无限长高（见 popup_render.go 的 maxCardH），所以这里做个约束。
	maxContentRunes = 100

	// 标题的字数上限。标题是单行且尾部省略，太长没意义。
	maxTitleRunes = 40

	// 弹窗自动关闭的默认秒数，也是老配置（没有 closeAfter 字段）的兜底值。
	defaultCloseAfter = 6

	// 自动关闭秒数的上下限。下限 1 秒：配 0 没有意义，只会让人以为没弹出来；
	// 上限 5 分钟：再长就不叫「弹一下」了，关不掉会挡着屏幕。
	minCloseAfter = 1
	maxCloseAfter = 300
)

// 弹窗可以停靠的位置。都是相对「工作区」而言的（工作区已排除任务栏）。
const (
	PosTopLeft      = "top-left"
	PosTopCenter    = "top-center"
	PosTopRight     = "top-right"
	PosMiddleLeft   = "middle-left"
	PosCenter       = "center"
	PosMiddleRight  = "middle-right"
	PosBottomLeft   = "bottom-left"
	PosBottomCenter = "bottom-center"
	PosBottomRight  = "bottom-right"
)

var validPositions = map[string]bool{
	PosTopLeft: true, PosTopCenter: true, PosTopRight: true,
	PosMiddleLeft: true, PosCenter: true, PosMiddleRight: true,
	PosBottomLeft: true, PosBottomCenter: true, PosBottomRight: true,
}

// positionOptions 是给日志/提示用的中文名，顺序与页面上的九宫格一致。
var positionOptions = []struct{ Key, Label string }{
	{PosTopLeft, "左上"}, {PosTopCenter, "上中"}, {PosTopRight, "右上"},
	{PosMiddleLeft, "左中"}, {PosCenter, "正中"}, {PosMiddleRight, "右中"},
	{PosBottomLeft, "左下"}, {PosBottomCenter, "下中"}, {PosBottomRight, "右下"},
}

// WorkWindow 是一段工作时段。在这段区间内，从 Start 开始每隔 Interval 分钟提醒一次。
// Start 和 End 都包含在内（End 那一刻也会提醒一次，如果你不希望提醒，把 End 往前挪）。
//
// Interval == 0 是特殊值，表示「仅一次」：只在这个时段的开始时刻提醒一次，
// 之后到 End 之前都不再提醒。这样「10 点提醒我去开会」这种一次性需求也能配。
//
// Title / Message 是这个时段专属的提示内容。留空则回落到全局的 Title / Message，
// 所以「想给每个时段配不同的提醒语」和「所有时段共用一条」两种用法都成立。
type WorkWindow struct {
	Start    string `json:"start"`             // "09:00"
	End      string `json:"end"`               // "11:00"
	Interval int    `json:"interval"`          // 分钟；0 = 仅一次
	Title    string `json:"title,omitempty"`   // 可选，留空用全局标题
	Message  string `json:"message,omitempty"` // 可选，留空用全局正文
}

// Once 表示这个时段只在开始时刻提醒一次。
func (w WorkWindow) Once() bool { return w.Interval <= 0 }

type Config struct {
	Enabled      bool         `json:"enabled"`      // 总开关
	AutoStart    bool         `json:"autoStart"`    // 开机自动启动（写当前用户注册表 Run 项）
	WeekdaysOnly bool         `json:"weekdaysOnly"` // 只在周一到周五提醒
	Title        string       `json:"title"`        // 默认通知标题，留空 = 弹窗不显示标题
	Message      string       `json:"message"`      // 默认通知正文
	Position     string       `json:"position"`     // 弹窗位置，见 PosXxx 常量
	CloseAfter   int          `json:"closeAfter"`   // 弹窗停留几秒后自动关闭（秒）
	Port         int          `json:"port"`         // 配置页面端口
	Windows      []WorkWindow `json:"windows"`      // 工作时段列表
}

// DefaultConfig 对应需求：9 点起整点提醒，11 点后午休，
// 13:30 起每 60 分钟一次到 20:30，弹窗出现在屏幕正中。
func DefaultConfig() Config {
	return Config{
		Enabled:      true,
		WeekdaysOnly: true,
		Title:        "该休息了",
		Message:      "站起来活动一下，喝口水，看看窗外 20 秒。",
		Position:     PosCenter,
		CloseAfter:   defaultCloseAfter,
		Port:         defaultPort,
		Windows: []WorkWindow{
			{Start: "09:00", End: "11:00", Interval: 60},
			{Start: "13:30", End: "20:30", Interval: 60},
		},
	}
}

func (w WorkWindow) ResolveTitle(fallback string) string {
	if s := strings.TrimSpace(w.Title); s != "" {
		return s
	}
	return fallback
}

func (w WorkWindow) ResolveMessage(fallback string) string {
	if s := strings.TrimSpace(w.Message); s != "" {
		return s
	}
	return fallback
}

func (c *Config) Normalize() {
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = defaultPort
	}
	if !validPositions[c.Position] {
		c.Position = PosCenter
	}
	// 老配置文件里没有 closeAfter 字段（解出来是 0），补成默认秒数；
	// 页面传 0 或负数也按同一逻辑处理，不用弹一堆「不能是 0」的错误。
	if c.CloseAfter <= 0 {
		c.CloseAfter = defaultCloseAfter
	}
	// 标题故意不给默认值：留空就是「弹窗顶部不显示标题」，
	// 有些人的提醒只要一句话就够，硬塞一个「该休息了」反而多余。
	c.Title = strings.TrimSpace(c.Title)
	c.Message = strings.TrimSpace(c.Message)
	if c.Message == "" {
		c.Message = "站起来活动一下，看看远处。"
	}
	if c.Windows == nil {
		c.Windows = []WorkWindow{}
	}
	for i := range c.Windows {
		c.Windows[i].Title = strings.TrimSpace(c.Windows[i].Title)
		c.Windows[i].Message = strings.TrimSpace(c.Windows[i].Message)
	}
}

func (c Config) Validate() error {
	if !validPositions[c.Position] {
		return fmt.Errorf("弹窗位置「%s」无效", c.Position)
	}
	// 0 / 负数已经在 Normalize 里补成默认值了，这里只拦「长得离谱」的。
	if c.CloseAfter > maxCloseAfter {
		return fmt.Errorf("弹窗停留时间最多 %d 秒", maxCloseAfter)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Title)); n > maxTitleRunes {
		return fmt.Errorf("默认标题有 %d 个字，超过上限 %d 个字", n, maxTitleRunes)
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(c.Message)); n > maxContentRunes {
		return fmt.Errorf("默认提示内容有 %d 个字，超过上限 %d 个字", n, maxContentRunes)
	}
	for i, w := range c.Windows {
		if _, _, err := parseHHMM(w.Start); err != nil {
			return fmt.Errorf("第 %d 个时段的开始时间「%s」无效：%w", i+1, w.Start, err)
		}
		sh, sm, _ := parseHHMM(w.Start)
		eh, em, err := parseHHMM(w.End)
		if err != nil {
			return fmt.Errorf("第 %d 个时段的结束时间「%s」无效：%w", i+1, w.End, err)
		}
		if sh*60+sm >= eh*60+em {
			return fmt.Errorf("第 %d 个时段：开始时间必须早于结束时间", i+1)
		}
		// Interval == 0 是合法的，表示「仅一次」；只有负数才算错。
		if w.Interval < 0 {
			return fmt.Errorf("第 %d 个时段：间隔不能是负数（0 表示仅一次）", i+1)
		}
		if w.Interval > 24*60 {
			return fmt.Errorf("第 %d 个时段：间隔不能超过 24 小时", i+1)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(w.Message)); n > maxContentRunes {
			return fmt.Errorf("第 %d 个时段的提示内容有 %d 个字，超过上限 %d 个字", i+1, n, maxContentRunes)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(w.Title)); n > maxTitleRunes {
			return fmt.Errorf("第 %d 个时段的标题有 %d 个字，超过上限 %d 个字", i+1, n, maxTitleRunes)
		}
	}
	return nil
}

// Store 负责配置的读写与热更新。
// 用 RWMutex 保护内存副本，HTTP handler 和调度 goroutine 并发读都不会出问题。
type Store struct {
	mu      sync.RWMutex
	cfg     Config
	path    string
	changed chan struct{}
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, changed: make(chan struct{}, 1)}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("配置文件 %s 格式错误：%w", path, err)
		}
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("配置文件 %s 内容无效：%w", path, err)
		}
		s.cfg = cfg
	case os.IsNotExist(err):
		s.cfg = DefaultConfig()
		if err := s.persist(s.cfg); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("读取配置文件失败：%w", err)
	}
	return s, nil
}

// Get 返回配置的深拷贝，避免调用方拿到内部切片的引用后乱改。
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.cfg
	out.Windows = append([]WorkWindow(nil), s.cfg.Windows...)
	return out
}

func (s *Store) Set(cfg Config) error {
	// 位置先校验再 Normalize：Normalize 会把非法值悄悄改成「正中」，
	// 那是为了兼容老配置文件（缺字段），但对接口调用方应该明确报错。
	if !validPositions[cfg.Position] {
		return fmt.Errorf("弹窗位置「%s」无效", cfg.Position)
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}

	// 开机启动是「配置之外的副作用」，必须在改内存之前做：
	// 写注册表失败就直接报错返回，内存里的配置保持原样，页面上的开关也会被回滚。
	s.mu.RLock()
	autoChanged := cfg.AutoStart != s.cfg.AutoStart
	s.mu.RUnlock()
	if autoChanged {
		if err := applyAutoStart(cfg.AutoStart); err != nil {
			return fmt.Errorf("设置开机启动失败：%w", err)
		}
	}

	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()

	if err := s.persist(cfg); err != nil {
		return err
	}
	s.notifyChanged()
	return nil
}

func (s *Store) persist(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入配置失败：%w", err)
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) notifyChanged() {
	select {
	case s.changed <- struct{}{}:
	default: // 已经有待处理的信号了，不重复塞
	}
}

func (s *Store) Changed() <-chan struct{} { return s.changed }
