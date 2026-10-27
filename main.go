package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

// appName 是本程序的身份证。探测端口时会比对它，
// 用来区分「端口上是自己的另一个实例」和「被别的程序占了」。
const appName = "restreminder"

var processStart = time.Now()

// shutdownCh 由页面上的「退出程序」按钮触发，close 后主流程走优雅退出。
var (
	shutdownOnce sync.Once
	shutdownCh   = make(chan struct{})
)

type StatusResp struct {
	App         string   `json:"app"`
	Pid         int      `json:"pid"`
	Port        int      `json:"port"`
	StartedAt   string   `json:"startedAt"`
	UptimeSec   int      `json:"uptimeSec"`
	Now         string   `json:"now"`
	Enabled     bool     `json:"enabled"`
	NextAt      string   `json:"nextAt,omitempty"`
	NextTitle   string   `json:"nextTitle,omitempty"`
	NextMessage string   `json:"nextMessage,omitempty"`
	InWindow    bool     `json:"inWindow"`
	TodayPoints []string `json:"todayPoints"`
}

func main() {
	log.SetFlags(log.Ltime | log.Lmsgprefix)
	log.SetPrefix("[提醒] ")

	port := flag.Int("port", 0, "配置页面端口，覆盖配置文件中的设置")
	noBrowser := flag.Bool("no-browser", false, "启动时不自动打开浏览器")
	silent := flag.Bool("silent", false, "静默启动：不打开浏览器、不弹启动提示、隐藏控制台（开机自启用）")
	preview := flag.String("preview", "", "把当前配置的文案渲染成一张 PNG 后退出（调版式用）")
	scale := flag.Float64("scale", 1.0, "配合 -preview：按多少倍 DPI 渲染")
	pvTitle := flag.String("preview-title", "", "配合 -preview：用指定的标题渲染")
	pvNoTitle := flag.Bool("preview-no-title", false, "配合 -preview：渲染不带标题的卡片")
	flag.Parse()

	if *silent {
		// 开机自启时由注册表拉起，屏幕上不该出现任何东西
		hideConsole()
		*noBrowser = true
	}

	store, err := NewStore(configPath())
	if err != nil {
		fatal("读取配置失败：%v", err)
	}

	cfg := store.Get()

	// 配置文件可能被整个目录拷到别的地方，注册表里的路径就过期了。
	// 每次启动按当前 exe 的真实路径重写一遍，代价是几毫秒，省掉「明明开了自启却不生效」。
	if cfg.AutoStart {
		if err := applyAutoStart(true); err != nil {
			log.Printf("开机启动项同步失败：%v", err)
		} else if cmd, ok := currentAutoStart(); ok {
			log.Printf("开机启动已开启：%s", cmd)
		}
	}

	if *preview != "" {
		title := cfg.Title
		switch {
		case *pvNoTitle:
			title = "" // 专门看「没标题」的版式
		case *pvTitle != "":
			title = *pvTitle
		}
		if err := writePreviewPNG(*preview, title, cfg.Message, *scale); err != nil {
			fatal("生成预览失败：%v", err)
		}
		log.Printf("已生成预览：%s", *preview)
		return
	}
	if *port > 0 && *port != cfg.Port {
		cfg.Port = *port
		if err := store.Set(cfg); err != nil {
			fatal("应用端口参数失败：%v", err)
		}
	}

	// 先把端口占住再往下走。不用 ListenAndServe 直接起，是因为它失败时只能在
	// goroutine 里 Fatalf 把进程瞬间干掉——双击启动时表现为"窗口闪一下就没了"，
	// 用户既看不到原因，也来不及打开配置页面。
	ln, actualPort := listenFirstFree(cfg.Port, *noBrowser)
	if ln == nil {
		return // listenFirstFree 内部已处理并退出
	}
	defer ln.Close()

	if actualPort != cfg.Port {
		log.Printf("端口 %d 已被别的程序占用，本次改用 %d（下次仍会先试 %d）", cfg.Port, actualPort, cfg.Port)
		cfg.Port = actualPort
	}

	// 提醒调度器独立跑在自己的 goroutine 里
	go RunScheduler(store)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/config", handleConfig(store))
	mux.HandleFunc("/api/status", handleStatus(store, cfg.Port))
	mux.HandleFunc("/api/test", handleTest(store))
	mux.HandleFunc("/api/shutdown", handleShutdown())

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("配置页面服务异常退出：%v", err)
			shutdownOnce.Do(func() { close(shutdownCh) })
		}
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	log.Printf("==================================================")
	log.Printf("  配置页面已就绪，在浏览器打开下面的地址：")
	log.Printf("      %s", url)
	log.Printf("  关掉浏览器不影响提醒；要退出程序请点页面上的「退出程序」。")
	log.Printf("==================================================")
	printTodaySchedule(cfg)

	if !*noBrowser {
		time.Sleep(250 * time.Millisecond)
		if err := openBrowser(url); err != nil {
			log.Printf("自动打开浏览器失败，请手动访问 %s", url)
		} else {
			log.Printf("已在默认浏览器打开配置页面")
		}

		// 双击启动的人手里只有一个控制台窗口，很容易怀疑"到底跑起来没有"。
		// 这里在桌面上弹一张卡片作为即时反馈，顺带验证弹窗通道本身是通的。
		if cfg.Enabled {
			if next, _, ok := NextReminder(cfg, time.Now()); ok {
				if err := notify("提醒已启动", "下次提醒 "+next.Format("15:04"), cfg.Position); err != nil {
					log.Printf("启动提示卡片没能弹出来：%v", err)
				}
			}
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		log.Println("收到退出信号，正在关闭……")
	case <-shutdownCh:
		log.Println("收到页面退出指令，正在关闭……")
		time.Sleep(250 * time.Millisecond) // 让 HTTP 响应先发出去
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	// 弹窗线程可能还阻塞在消息循环里，Shutdown 管不到它，直接退出最干净。
	os.Exit(0)
}

// portScanSpan 重复启动时往后探测的端口跨度。上一个实例可能因为端口冲突顺延过，
// 所以判断"是不是已经在跑"不能只看配置里的那一个端口。
const portScanSpan = 20

// listenFirstFree 从 want 开始最多试 21 个端口，返回第一个能监听的。
// 返回 nil 表示不该继续启动（要么本程序已经在跑，要么彻底没端口可用）。
func listenFirstFree(want int, noBrowser bool) (net.Listener, int) {
	if ln, err := net.Listen("tcp", listenAddr(want)); err == nil {
		return ln, want
	}

	// 端口被占。先在整段候选区间里找有没有自己的实例在跑——
	// 只测 want 这一个端口是不够的：如果上一次启动时 want 被别的程序占着，
	// 那个实例会顺延到 want+1，只测 want 就会误判成"没在跑"，
	// 于是再起一个，桌面上变成两张卡片、双份提醒。
	if port, ok := findRunningInstance(want, portScanSpan); ok {
		announceRunning(port, noBrowser)
		return nil, 0
	}

	// 确认没有实例在跑，那 this 端口就是被别的程序占着，往后找空位
	for p := want + 1; p <= want+portScanSpan; p++ {
		if ln, err := net.Listen("tcp", listenAddr(p)); err == nil {
			return ln, p
		}
	}
	fatal("端口 %d～%d 都被占用了，请用 -port 换一个，例如 restreminder.exe -port 9000",
		want, want+portScanSpan)
	return nil, 0
}

// findRunningInstance 在 [from, from+span] 里并发找自己的实例，返回最小的可用端口。
// 并发是必要的：串行探测遇到不响应 HTTP 的端口（比如别的程序只 listen 不应答）
// 每个都要等满超时，21 个端口能拖到十几秒；并发后总耗时就是一次超时。
func findRunningInstance(from, span int) (int, bool) {
	type hit struct {
		port int
		ok   bool
	}
	ch := make(chan hit, span+1)
	for p := from; p <= from+span; p++ {
		go func(p int) { ch <- hit{p, probeSelf(p)} }(p)
	}
	best := 0
	for i := 0; i <= span; i++ {
		if h := <-ch; h.ok && (best == 0 || h.port < best) {
			best = h.port
		}
	}
	return best, best > 0
}

// announceRunning 告诉用户"已经在跑了"，并把页面送到他面前。
func announceRunning(port int, noBrowser bool) {
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	log.Printf("==================================================")
	log.Printf("  程序已经在运行了，不用再启动一个。")
	log.Printf("  配置页面：%s", url)
	log.Printf("==================================================")
	if !noBrowser {
		if err := openBrowser(url); err != nil {
			log.Printf("打开浏览器失败，请手动访问 %s", url)
		} else {
			log.Printf("已在默认浏览器打开配置页面")
		}
	}
	holdIfDoubleClicked()
}

func listenAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// probeSelf 判断某端口上跑的是不是本程序：读 /api/status 里的 app 字段认亲。
// 只连本机回环地址，超时 500ms，不会拖慢启动。
func probeSelf(port int) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/status", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var probe struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&probe); err != nil {
		return false
	}
	return probe.App == appName
}

// fatal 打印错误后退出。双击启动时会把窗口停住，防止错误提示一闪而过。
func fatal(format string, args ...any) {
	// -silent 启动时控制台是藏起来的，出了错必须翻出来，否则用户只会看到"程序没反应"
	showConsole()
	log.Printf("启动失败："+format, args...)
	holdIfDoubleClicked()
	os.Exit(1)
}

// configPath 把配置放在可执行文件旁边，方便整个目录拷走即用。
func configPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

func printTodaySchedule(cfg Config) {
	sched := TodaySchedule(cfg, time.Now())
	if len(sched) == 0 {
		log.Printf("今天没有可用的提醒时刻（可能已暂停，或今天是周末）")
		return
	}
	ss := make([]string, 0, len(sched))
	for _, p := range sched {
		ss = append(ss, p.At.Format("15:04"))
	}
	log.Printf("今天的提醒时刻：%s", strings.Join(ss, "  "))
	log.Printf("弹窗位置：%s", positionLabel(cfg.Position))
	if next, win, ok := NextReminder(cfg, time.Now()); ok {
		log.Printf("下一次：%s → %s", next.Format("01-02 15:04"), win.ResolveMessage(cfg.Message))
	}
}

func positionLabel(pos string) string {
	for _, p := range positionOptions {
		if p.Key == pos {
			return p.Label
		}
	}
	return pos
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}

func handleConfig(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, store.Get())

		case http.MethodPost:
			var cfg Config
			body := io.LimitReader(r.Body, 1<<20)
			if err := json.NewDecoder(body).Decode(&cfg); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON：" + err.Error()})
				return
			}
			if err := store.Set(cfg); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "不支持的请求方法", http.StatusMethodNotAllowed)
		}
	}
}

func handleStatus(store *Store, port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := store.Get()
		now := time.Now()

		resp := StatusResp{
			App:       appName,
			Pid:       os.Getpid(),
			Port:      port,
			StartedAt: processStart.Format(time.RFC3339),
			UptimeSec: int(now.Sub(processStart).Seconds()),
			Now:       now.Format(time.RFC3339),
			Enabled:   cfg.Enabled,
			InWindow:  InWindow(cfg, now),
		}
		if next, win, ok := NextReminder(cfg, now); ok {
			resp.NextAt = next.Format(time.RFC3339)
			resp.NextTitle = win.ResolveTitle(cfg.Title)
			resp.NextMessage = win.ResolveMessage(cfg.Message)
		}
		for _, p := range TodayPoints(cfg, now) {
			resp.TodayPoints = append(resp.TodayPoints, p.Format("15:04"))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// handleTest 立刻弹一条。请求体可以带 {title, message, position} 覆盖当前配置——
// 这样页面上「试一下」能直接验证还没保存的文案和位置，不用先保存再看效果。
func handleTest(store *Store) http.HandlerFunc {
	type testReq struct {
		Title    string `json:"title"`
		Message  string `json:"message"`
		Position string `json:"position"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "不支持的请求方法", http.StatusMethodNotAllowed)
			return
		}

		cfg := store.Get()

		var req testReq
		if r.Body != nil {
			// 空 body 会返回 io.EOF，这属于正常情况（直接用默认文案），忽略即可
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = cfg.Title
		}
		msg := strings.TrimSpace(req.Message)
		if msg == "" {
			msg = cfg.Message
		}
		pos := req.Position
		if !validPositions[pos] {
			pos = cfg.Position
		}

		if err := notify(title, msg, pos); err != nil {
			log.Printf("测试通知失败：%v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("已发送测试通知（%s）：%s", positionLabel(pos), msg)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// handleShutdown 让页面能给程序发退出指令。
// 之所以需要它：程序是常驻后台跑的，用户手里只有一个浏览器标签页，
// 关掉标签页并不会结束进程，之前只能用任务管理器强杀。
func handleShutdown() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "不支持的请求方法", http.StatusMethodNotAllowed)
			return
		}
		log.Printf("收到页面发来的退出指令")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		shutdownOnce.Do(func() { close(shutdownCh) })
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
