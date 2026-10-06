package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/energye/systray"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const appVersion = "1.0.0"

// 开机自启快捷方式落在启动文件夹（reg.exe 在本机被安全策略拦截，故不写注册表）。
const startupLnkName = "AGIEGG.lnk"

// EngineStatus 是单个引擎的状态快照，供前端渲染。
type EngineStatus struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Running  bool   `json:"running"`  // 端口在监听 / 进程存活
	Online   bool   `json:"online"`   // /status 可访问
	Accounts int    `json:"accounts"` // 账号数
	URL      string `json:"url"`      // API 基址
	LogTail  string `json:"log_tail"` // 引擎日志尾部

	// 以下来自 /status 解析
	Total        int           `json:"total"`
	Healthy      int           `json:"healthy"`
	Cooling      int           `json:"cooling"`
	Disabled     int           `json:"disabled"`
	Credits      int           `json:"credits"`       // 剩余积分合计
	CreditCap    int           `json:"credit_cap"`    // 总额度合计
	CreditsKnown bool          `json:"credits_known"` // false = 引擎未上报额度（显示「-」而非 0）
	AccountList  []AccountInfo `json:"account_list"`
	HealthyPct   int           `json:"healthy_pct"` // 健康账号占比 0-100
	CreditPct    int           `json:"credit_pct"`  // 剩余额度占比 0-100

	Port      int    `json:"port"`       // 监听端口
	PID       int    `json:"pid"`        // 本进程拉起的 PID（0 = 外部实例）
	PanelURL  string `json:"panel_url"`  // 引擎自带面板地址，空 = 该引擎没有面板
	PanelOK   bool   `json:"panel_ok"`   // 面板可访问
	Uptime    string `json:"uptime"`     // 本次由本进程拉起后的运行时长
	Err       string `json:"err"`        // 最近一次接口错误
	StartedAt string `json:"started_at"` // 本进程拉起该引擎的时刻
}

// StatusResp 是 Status() 的返回结构。
type StatusResp struct {
	WB        EngineStatus `json:"wb"`
	Autostart bool         `json:"autostart"`
	Version   string       `json:"version"`
	StartedAt string       `json:"started_at"`
	Root      string       `json:"root"`
	UIURL     string       `json:"ui_url"`   // 内置网页界面地址（不依赖 WebView2）
	WebMode   bool         `json:"web_mode"` // true = 当前就是浏览器模式（无原生窗口）

	// 全局汇总
	Uptime       string `json:"uptime"`        // AGIEGG 自身运行时长
	UptimeSec    int64  `json:"uptime_sec"`
	AllAccounts  int    `json:"all_accounts"`  // 账号总数
	AllHealthy   int    `json:"all_healthy"`   // 健康账号数
	AllCredits   int    `json:"all_credits"`   // 剩余积分合计
	AllCreditCap int    `json:"all_credit_cap"`
	AllRunning   int    `json:"all_running"`   // 引擎是否在运行（0/1）
	AnyIssue     bool   `json:"any_issue"`     // 有引擎未运行，或存在冷却/禁用账号
}

// App 是 Wails 绑定的主结构体，也是整个启动器的中枢。
type App struct {
	ctx          context.Context
	conf         Conf
	runtimeRoot  string
	wb           *Engine
	startedAt    time.Time
	autostartLnk string
	ready        atomic.Bool // 资产解包 + 引擎构建完成

	// 内置网页界面（不依赖 WebView2 的备份通路）
	uiSrv    *http.Server
	uiPort   int
	uiURL    string
	webMode  atomic.Bool // --web：完全不起 Wails 窗口
	crashLog string      // WebView2 崩溃信息（供界面提示）

	// 引擎面板探测缓存（wb2api 的面板挂在 /panel/，不能硬编码成 /）
	panelMu    sync.Mutex
	panelCache map[string]panelProbe

	trayTipMu  sync.Mutex
	trayTip    string      // 最近一次写入托盘的提示，避免重复刷新
	winTitle   string      // 最近一次写入的窗口标题，避免重复刷新
	trayReady  atomic.Bool // 托盘已注册（未注册时调用 systray API 会失败）
}

func newApp() *App {
	return &App{
		startedAt:  time.Now(),
		panelCache: make(map[string]panelProbe),
	}
}

// ---------------------------------------------------------------- 生命周期

// bootstrap 解包资产 → 构建引擎 → 自动拉起。
// 与 UI 无关，由 main 在 wails.Run 之前调用（UI 挂了也要保住自动任务）。
func (a *App) bootstrap() error {
	a.conf = loadConf()
	if err := a.extractAssets(); err != nil {
		// 解包失败也继续构建引擎（路径已确定），错误上抛给调用方记录。
		a.buildEngines()
		a.ready.Store(true)
		return err
	}
	a.buildEngines()
	a.ready.Store(true)

	// 自动拉起引擎：签到/活动/保活等任务由引擎内置调度自行执行。
	_ = a.wb.start()
	return nil
}

// startup 在 Wails 窗口初始化时调用（引擎通常已在 bootstrap 拉起）。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	if !a.ready.Load() {
		if err := a.bootstrap(); err != nil {
			fmt.Println("启动异常:", err)
		}
	}
}

// shutdown 在退出时清理引擎进程。
func (a *App) shutdown(ctx context.Context) {
	if a.wb != nil {
		a.wb.stop()
	}
}

// ---------------------------------------------------------------- 资产解包

// extractAssets 把内嵌的引擎目录解到 exe 同级的可写目录。
// 引擎需要写 data/state.json 与日志，不能直接在只读的 embed FS 上运行。
func (a *App) extractAssets() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(exe), "agiegg_runtime")
	a.runtimeRoot = root
	for _, sub := range []string{"wb"} {
		dst := filepath.Join(root, sub)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := a.copyEmbedDir("embedded/"+sub, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyEmbedDir 递归复制 embed 子目录到 dst；
// 已存在且大小一致的文件跳过（保留用户 state / auths / 自定义 config）。
func (a *App) copyEmbedDir(prefix string, dst string) error {
	return fs.WalkDir(embeddedFS, prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")
		if rel == "" {
			return nil // prefix 自身
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, e := embeddedFS.ReadFile(path)
		if e != nil {
			return e
		}
		if fi, e2 := os.Stat(target); e2 == nil && fi.Size() == int64(len(data)) {
			return nil
		}
		mode := os.FileMode(0o644)
		if strings.EqualFold(filepath.Ext(path), ".exe") {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
}

// buildEngines 依据配置构造引擎实例。
func (a *App) buildEngines() {
	root := a.runtimeRoot
	a.wb = &Engine{
		Key:        "wb",
		Title:      "WorkBuddy 账号池",
		Dir:        filepath.Join(root, "wb"),
		Exe:        filepath.Join(root, "wb", "wb2api.exe"),
		ListenAddr: "127.0.0.1:" + a.conf.WBPort,
		APIKey:     a.conf.WBKey,
		EnvKeyName: "WB2A_API_KEY",
		EnvListen:  "WB2A_LISTEN",
		logPath:    filepath.Join(root, "wb", "engine.log"),
	}
}

// ---------------------------------------------------------------- 状态

func (a *App) engineByName(name string) *Engine {
	switch strings.ToLower(name) {
	case "wb", "workbuddy", "wb2api":
		return a.wb
	}
	return nil
}

func (a *App) isAll(name string) bool {
	n := strings.ToLower(name)
	return n == "all" || n == ""
}

func (a *App) snap(e *Engine, url string) EngineStatus {
	if e == nil {
		return EngineStatus{}
	}
	running := e.running()
	st := EngineStatus{
		Key:      e.Key,
		Title:    e.Title,
		Running:  running,
		URL:      url,
		LogTail:  tailFile(e.logPath, 8),
		Port:     e.Port(),
		PID:      e.PID(),
	}
	if t := e.StartedAt(); !t.IsZero() {
		st.StartedAt = t.Format("15:04:05")
		st.Uptime = fmtDuration(time.Since(t))
	}
	st.PanelURL, st.PanelOK = a.panelURL(e)

	if !running {
		st.Err = "引擎未运行"
		return st
	}
	s := e.fetch()
	if !s.Online {
		st.Err = "/status 不可达（端口占用或鉴权失败）"
		return st
	}
	st.Online = true
	st.Total, st.Healthy, st.Cooling, st.Disabled = s.Total, s.Healthy, s.Cooling, s.Disabled
	st.Credits, st.CreditCap, st.CreditsKnown = s.Credits, s.CreditCap, s.CreditsKnown
	st.AccountList = s.Accounts
	st.Accounts = len(s.Accounts)
	if st.Total > 0 {
		st.HealthyPct = st.Healthy * 100 / st.Total
	}
	if st.CreditCap > 0 {
		st.CreditPct = st.Credits * 100 / st.CreditCap
	}
	return st
}

// Accounts 返回账号明细（wb / all）。
// 与 Status 分开，便于界面只在展开账号列表时才拉取。
func (a *App) Accounts(name string) []AccountInfo {
	if !a.ready.Load() {
		return nil
	}
	if a.isAll(name) {
		return append([]AccountInfo{}, a.wb.fetch().Accounts...)
	}
	e := a.engineByName(name)
	if e == nil {
		return nil
	}
	return e.fetch().Accounts
}

// Status 返回引擎状态 + 汇总 + 自启状态，前端每 5s 轮询。
func (a *App) Status() StatusResp {
	if !a.ready.Load() {
		return StatusResp{Version: appVersion}
	}
	wb := a.snap(a.wb, "http://"+a.wb.ListenAddr+"/")

	r := StatusResp{
		WB:        wb,
		Autostart: a.IsAutostart(),
		Version:   appVersion,
		StartedAt: a.startedAt.Format("2006-01-02 15:04:05"),
		Root:      a.runtimeRoot,
		UIURL:     a.uiURL,
		WebMode:   a.webMode.Load(),
	}
	for _, s := range []EngineStatus{wb} {
		if s.Running {
			r.AllRunning++
		}
		r.AllAccounts += s.Accounts
		r.AllHealthy += s.Healthy
		r.AllCredits += s.Credits
		r.AllCreditCap += s.CreditCap
		if !s.Running || !s.Online || s.Cooling > 0 || s.Disabled > 0 {
			r.AnyIssue = true
		}
	}
	r.UptimeSec = int64(time.Since(a.startedAt).Seconds())
	r.Uptime = fmtDuration(time.Since(a.startedAt))

	// 托盘提示与窗口标题直接显示关键数字，不用展开界面也能看到池子状态。
	a.updateTrayTooltip(r)
	a.updateWindowTitle(r)
	return r
}

// updateWindowTitle 把概况写进窗口标题。窗口最小化或收起到托盘后，
// 悬停任务栏就能看到「几个引擎在跑、多少账号、多少积分」，省得点开窗口。
func (a *App) updateWindowTitle(r StatusResp) {
	if !a.hasWindow() {
		return
	}
	credits := "-"
	if r.AllCredits > 0 || r.AllCreditCap > 0 {
		credits = strconv.Itoa(r.AllCredits)
	}
	state := "已停止"
	if r.AllRunning > 0 {
		state = "运行中"
	}
	title := fmt.Sprintf("AGIEGG %s · %d 账号 · %s 积分",
		state, r.AllAccounts, credits)

	a.trayTipMu.Lock()
	same := a.winTitle == title
	a.winTitle = title
	a.trayTipMu.Unlock()
	if same {
		return
	}
	wruntime.WindowSetTitle(a.ctx, title)
}

// fmtDuration 把时长格式化为「2 小时 14 分」这样的中文短串。
func fmtDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	sec := int(d.Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", sec)
	case sec < 3600:
		return fmt.Sprintf("%d 分 %02d 秒", sec/60, sec%60)
	case sec < 86400:
		return fmt.Sprintf("%d 小时 %02d 分", sec/3600, (sec%3600)/60)
	default:
		return fmt.Sprintf("%d 天 %02d 小时", sec/86400, (sec%86400)/3600)
	}
}

// ---------------------------------------------------------------- 面板探测

type panelProbe struct {
	url string
	ok  bool
	at  time.Time
}

// panelURL 探测引擎自带的 Web 面板地址。
//
// 必须探测而不是硬编码：wb2api 的面板在 /panel/，而根路径 / 直接 404
// （实测：打开 / 就是「管理面板打不开」的原因）。
// 结果缓存 60 秒，避免 5s 一次的状态轮询反复发探测请求。
func (a *App) panelURL(e *Engine) (string, bool) { return a.probePanel(e, false) }

// probePanel force=true 时忽略缓存重新探测（用户主动点「打开面板」时用）。
func (a *App) probePanel(e *Engine, force bool) (string, bool) {
	if e == nil {
		return "", false
	}
	a.panelMu.Lock()
	if p, ok := a.panelCache[e.Key]; ok && !force {
		// 成功结果可缓存久一点；失败只缓存 10 秒——引擎刚拉起时面板可能
		// 尚未注册路由（实测启动后约 10s 内 /panel/ 会失败），短缓存能很快自愈。
		ttl := 60 * time.Second
		if !p.ok {
			ttl = 10 * time.Second
		}
		if time.Since(p.at) < ttl {
			a.panelMu.Unlock()
			return p.url, p.ok
		}
	}
	a.panelMu.Unlock()

	url, ok := "", false
	for _, path := range []string{"/panel/", "/"} {
		u := e.baseURL() + path
		if probeHTTP(u) {
			url, ok = u, true
			break
		}
	}

	a.panelMu.Lock()
	a.panelCache[e.Key] = panelProbe{url: url, ok: ok, at: time.Now()}
	a.panelMu.Unlock()
	return url, ok
}

// probeHTTP 判断 URL 是否返回可用内容（2xx/3xx 视为可用，4xx/5xx 视为无页面）。
func probeHTTP(u string) bool {
	cl := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := cl.Get(u)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

// ---------------------------------------------------------------- 引擎控制

// Start 启动引擎（wb / all）。
func (a *App) Start(name string) error {
	if !a.ready.Load() {
		return fmt.Errorf("尚未就绪")
	}
	if a.isAll(name) {
		return a.wb.start()
	}
	e := a.engineByName(name)
	if e == nil {
		return fmt.Errorf("未知引擎: %s", name)
	}
	return e.start()
}

// Stop 停止指定引擎。
func (a *App) Stop(name string) error {
	if !a.ready.Load() {
		return fmt.Errorf("尚未就绪")
	}
	if a.isAll(name) {
		a.wb.stop()
		return nil
	}
	e := a.engineByName(name)
	if e == nil {
		return fmt.Errorf("未知引擎: %s", name)
	}
	e.stop()
	return nil
}

// Restart 重启指定引擎。
func (a *App) Restart(name string) error {
	if err := a.Stop(name); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	return a.Start(name)
}

// ---------------------------------------------------------------- 一键任务

// Signin 执行签到（signin.exe 无参即签全部账号）。
func (a *App) Signin(name string) string {
	if !a.ready.Load() {
		return "尚未就绪"
	}
	if a.isAll(name) {
		return "=== WorkBuddy ===\n" + a.runTool(a.wb, "signin.exe", 120*time.Second)
	}
	e := a.engineByName(name)
	if e == nil {
		return "未知引擎: " + name
	}
	return a.runTool(e, "signin.exe", 120*time.Second)
}

// SigninAll 全部账号签到。
func (a *App) SigninAll() string { return a.Signin("all") }

// Credits 查询积分（credit.exe -pretty）。
func (a *App) Credits(name string) string {
	if !a.ready.Load() {
		return "尚未就绪"
	}
	if a.isAll(name) {
		return "=== WorkBuddy ===\n" + a.runTool(a.wb, "credit.exe", 40*time.Second, "-pretty")
	}
	e := a.engineByName(name)
	if e == nil {
		return "未知引擎: " + name
	}
	return a.runTool(e, "credit.exe", 40*time.Second, "-pretty")
}

// runTool 在引擎工作目录执行维护工具，返回带退出码的 UTF-8 输出。
func (a *App) runTool(e *Engine, tool string, timeout time.Duration, args ...string) string {
	path := filepath.Join(e.Dir, tool)
	if _, err := os.Stat(path); err != nil {
		return fmt.Sprintf("[跳过] 未找到 %s", tool)
	}
	out, code := runCmd(e.Dir, timeout, path, args...)
	if strings.TrimSpace(out) == "" {
		out = "(无输出)"
	}
	return fmt.Sprintf("[退出码 %d]\n%s", code, out)
}

// ---------------------------------------------------------------- 面板 / 窗口

// PanelInfo 把「窗口内打开面板」所需的信息交给前端。
//
// 面板登录态是面板自己存进 localStorage 的 wb2api.key（Authorization: Bearer），
// 宿主页与面板经反代同源，前端预置该键后打开即是已登录状态，无需再输密钥。
func (a *App) PanelInfo() map[string]string {
	name := "WorkBuddy 控制台"
	if a.wb != nil {
		name = a.wb.Title + " 控制台"
	}
	return map[string]string{
		"url":  "/panel/",
		"key":  a.conf.WBKey,
		"name": name,
	}
}

// OpenPanel 在默认浏览器打开引擎自带的管理面板。
//
// 之前这里硬编码打开根路径 /，而 wb2api 的面板实际挂在 /panel/（/ 直接 404），
// 这就是「管理面板打不开」的根因。现在改为探测真实路径；
// 探测不到面板时返回明确错误，让界面给出替代入口。
// 界面主按钮已改为窗口内打开（iframe + 反代），本方法保留为「浏览器打开」兜底。
func (a *App) OpenPanel(name string) error {
	e := a.engineByName(name)
	if e == nil {
		return fmt.Errorf("未知引擎: %s", name)
	}
	if !e.running() {
		return fmt.Errorf("%s 未运行，请先启动", e.Title)
	}
	url, ok := a.probePanel(e, true)
	if !ok || url == "" {
		return fmt.Errorf("%s 没有内置管理面板（该引擎只提供 API）", e.Title)
	}
	return openBrowser(url)
}

// OpenWebUI 在默认浏览器打开 AGIEGG 自己的网页界面。
func (a *App) OpenWebUI() error {
	if a.uiPort == 0 {
		return fmt.Errorf("网页界面未启动")
	}
	return openBrowser(a.uiURL)
}

// OpenFolder 打开运行时目录（auths / config 所在）。
func (a *App) OpenFolder() error {
	if a.runtimeRoot == "" {
		return fmt.Errorf("运行时目录未知")
	}
	cmd := exec.Command("explorer.exe", a.runtimeRoot)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// hasWindow 判断是否存在原生窗口（web 模式下没有，窗口类调用须跳过）。
func (a *App) hasWindow() bool {
	return a.ctx != nil && !a.webMode.Load()
}

func (a *App) ShowWindow() {
	if a.hasWindow() {
		wruntime.WindowShow(a.ctx)
		return
	}
	// 无原生窗口时退化为打开网页界面，语义上仍然是「把界面显示出来」。
	_ = a.OpenWebUI()
}

func (a *App) HideWindow() {
	if a.hasWindow() {
		wruntime.WindowHide(a.ctx)
	}
}

func (a *App) QuitApp() {
	if a.hasWindow() {
		wruntime.Quit(a.ctx)
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		a.shutdown(a.ctx)
		os.Exit(0)
	}()
}

// ---------------------------------------------------------------- 开机自启

func (a *App) startupLnkPath() string {
	if a.autostartLnk != "" {
		return a.autostartLnk
	}
	startup := filepath.Join(os.Getenv("APPDATA"),
		"Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	return filepath.Join(startup, startupLnkName)
}

// IsAutostart 启动文件夹是否存在 AGIEGG 快捷方式。
func (a *App) IsAutostart() bool {
	_, err := os.Stat(a.startupLnkPath())
	return err == nil
}

// SetAutostart 创建或删除启动文件夹快捷方式。
// 用 powershell -Command 内联传参（走 argv UTF-16），避开 .ps1 的 GBK 解码问题。
func (a *App) SetAutostart(on bool) error {
	lnk := a.startupLnkPath()

	if !on {
		if _, err := os.Stat(lnk); err == nil {
			return os.Remove(lnk)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// --web --hidden：开机时静默驻留托盘 + 网页界面。
	// 不用原生窗口，因为 WebView2 初始化失败时会硬退出并弹框，每次开机都弹一次很烦。
	script := fmt.Sprintf(
		"$ws=New-Object -ComObject WScript.Shell; "+
			"$sc=$ws.CreateShortcut('%s'); "+
			"$sc.TargetPath='%s'; "+
			"$sc.WorkingDirectory='%s'; "+
			"$sc.Description='AGIEGG Launcher'; "+
			"$sc.Arguments='--web --hidden'; "+
			"$sc.Save()",
		lnk, exe, filepath.Dir(exe),
	)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("创建自启快捷方式失败: %v | %s", err, toUTF8(out))
	}
	if _, err := os.Stat(lnk); err != nil {
		return fmt.Errorf("快捷方式未生成: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------- 系统托盘

// updateTrayTooltip 把池子概况写进托盘提示，不展开窗口也能看到关键数字。
// 内容变化才真正刷新，避免每 5 秒无谓地动用托盘 API。
func (a *App) updateTrayTooltip(r StatusResp) {
	credits := "-"
	if r.AllCreditCap > 0 {
		credits = fmt.Sprintf("%d / %d", r.AllCredits, r.AllCreditCap)
	} else if r.AllCredits > 0 {
		credits = strconv.Itoa(r.AllCredits)
	}
	state := "已停止"
	if r.AllRunning > 0 {
		state = "运行中"
	}
	tip := fmt.Sprintf("AGIEGG WorkBuddy 池%s，账号 %d（健康 %d），积分 %s",
		state, r.AllAccounts, r.AllHealthy, credits)

	a.trayTipMu.Lock()
	same := a.trayTip == tip
	a.trayTip = tip
	a.trayTipMu.Unlock()
	if same {
		return
	}
	if a.trayReady.Load() {
		systray.SetTooltip(tip)
	}
}

func (a *App) systrayReady() {
	systray.SetIcon(iconICO)
	systray.SetTooltip("AGIEGG · 账号池启动器")
	a.trayReady.Store(true)

	mShow := systray.AddMenuItem("显示窗口", "打开 AGIEGG 控制窗口")
	mHide := systray.AddMenuItem("隐藏窗口", "收起到系统托盘")
	mWeb := systray.AddMenuItem("在浏览器打开界面", "用默认浏览器打开（WebView2 异常时的备用通路）")
	systray.AddSeparator()
	mStart := systray.AddMenuItem("启动引擎", "启动 WorkBuddy 账号池")
	mStop := systray.AddMenuItem("停止引擎", "停止 WorkBuddy 账号池")
	mSign := systray.AddMenuItem("立即签到", "签到处全部账号")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "完全退出 AGIEGG")

	mShow.Click(func() { a.ShowWindow() })
	mHide.Click(func() { a.HideWindow() })
	mWeb.Click(func() { _ = a.OpenWebUI() })
	mStart.Click(func() {
		if a.ready.Load() {
			_ = a.Start("all")
		}
	})
	mStop.Click(func() {
		if a.ready.Load() {
			_ = a.Stop("all")
		}
	})
	mSign.Click(func() {
		if a.ready.Load() {
			_ = a.SigninAll()
		}
	})
	mQuit.Click(func() { a.QuitApp() })
}

func (a *App) systrayExit() {
	// 托盘退出时确保引擎收尾（正常路径由 wails OnShutdown 处理）
	a.shutdown(a.ctx)
}
