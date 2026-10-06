package main

import (
	"context"
	"io/fs"
	"log"
	"os"
	"syscall"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

func main() {
	app := newApp()

	// --hidden：开机自启时只驻留托盘，不弹窗。
	// --web   ：完全不起原生窗口，用默认浏览器承载界面。
	// --window：强制使用原生窗口（即使用户环境被判定为沙箱）。
	hidden := false
	webMode := os.Getenv("AGIEGG_WEB") == "1"
	forceWindow := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--hidden", "-hidden":
			hidden = true
		case "--web", "-web":
			webMode = true
		case "--window", "-window":
			forceWindow = true
		}
	}

	// 隔离沙箱（lsbox）会给子孙进程注入未签名 DLL，msedgewebview2.exe 因此被
	// 代码完整性检查杀掉（本机事件日志可见 tsbx.dll 被拒）→ Wails 直接 os.Exit(-1)。
	// 这种环境下自动改用网页模式，避免每次启动都弹崩溃框。
	if !forceWindow && inIsolatedSandbox() {
		webMode = true
	}
	app.webMode.Store(webMode)

	// UI 子文件系统：让 index.html 位于 FS 根，静态资源才能以 / 命中。
	uiSub, err := fs.Sub(uiFS, "frontend/dist")
	if err != nil {
		log.Fatalf("UI 子文件系统失败: %v", err)
	}

	// 先拉起引擎（与 UI 无关），这样即使界面进程崩溃，自动任务/签到照常运行。
	if err := app.bootstrap(); err != nil {
		log.Printf("引擎启动异常: %v", err)
	}

	// 内置网页界面：既是 Wails 窗口的备份通路，也是 --web 模式的唯一界面。
	// 必须在 wails.Run 之前启动——崩溃后用户仍然能从这里操作。
	if err := app.startUIServer(uiSub); err != nil {
		log.Printf("网页界面未启动: %v", err)
	}

	// 系统托盘：Register 建窗口 + nativeLoop 取消息必须在同一 goroutine 连续执行，
	// 因此整体放进一个 goroutine（不能拆成 RunWithExternalLoop 的两个回调）。
	go systray.Run(app.systrayReady, app.systrayExit)

	// 网页模式：不碰 WebView2，因此完全不受浏览器进程注入/代码完整性拦截影响。
	if webMode {
		log.Printf("网页模式：界面地址 %s", app.uiURL)
		if !hidden { // --hidden 时不弹浏览器，只驻留托盘 + 后台服务
			app.openBrowserBestEffort()
		}
		select {} // 托盘与 HTTP 服务各自在 goroutine 里跑，主协程阻塞即可
	}

	// 1200×800：默认开窗尺寸（3:2 比例，内容区约 1184×757），窗口可自由缩放/最大化；
	// Min 只钳布局下限（窄于 900px 内容宽时前端折成单列）；fitToScreen 保证
	// 低分屏（1366×768 等）按默认尺寸打开也不溢出。
	winW, winH := fitToScreen(1200, 800)

	err = wails.Run(&options.App{
		Title:             "AGIEGG · 账号池启动器",
		Width:             winW,
		Height:            winH,
		MinWidth:          780,
		MinHeight:         560,
		HideWindowOnClose: true, // 关闭窗口 → 收起到托盘，而非退出
		StartHidden:       hidden,
		// 浅色界面：窗口底色与前端 --bg 对齐，避免加载瞬间闪一块深色。
		BackgroundColour:  options.NewRGBA(246, 248, 250, 255),
		AssetServer: &assetserver.Options{
			Assets: uiSub,
			// 界面资源未命中的路径(含全部非 GET)交给同一路由:
			// 静态文件走本地,其余反代引擎——管理面板因此在窗口内同源可见,
			// 不再需要跳浏览器。/wails/* 运行时路径由 Wails 内部先行处理,不会到这。
			Handler: app.buildRootHandler(uiSub),
		},
		OnStartup:  app.startup,
		OnShutdown: app.onShutdown,
		Bind:       []interface{}{app},
		Windows:    windowsOptions(app),
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "agiegg-single-instance",
			OnSecondInstanceLaunch: func(data options.SecondInstanceData) {
				// 已在运行：把已有实例界面唤出，本进程随后退出。
				app.ShowWindow()
			},
		},
	})

	// 走到这里说明窗口循环结束。注意：WebView2 浏览器进程崩溃时 Wails 会
	// 直接 os.Exit(-1)（frontend.go:539），根本不会返回，所以这里只处理正常退出。
	if err != nil {
		log.Printf("界面启动失败(引擎仍在运行, 可用网页界面 %s): %v", app.uiURL, err)
		select {}
	}
}

// fitToScreen 把默认窗口尺寸钳制到主屏内（高度给任务栏 + 窗口标题留约 14% 余量），
// 避免 1366×768 这类低分屏被默认尺寸顶出屏幕。取不到屏幕尺寸时原样返回。
func fitToScreen(w, h int) (int, int) {
	gm := syscall.NewLazyDLL("user32.dll").NewProc("GetSystemMetrics")
	sw, _, _ := gm.Call(0) // SM_CXSCREEN
	sh, _, _ := gm.Call(1) // SM_CYSCREEN
	mw, mh := int(sw)*95/100, int(sh)*86/100
	if mw > 400 && w > mw {
		w = mw
	}
	if mh > 300 && h > mh {
		h = mh
	}
	return w, h
}

// inIsolatedSandbox 判断当前进程是否由 WorkBuddy 的隔离沙箱（lsbox）派生。
// 沙箱通过注入 tsbx.dll 实现管控，而 msedgewebview2.exe 拒绝加载未签名 DLL，
// 因此这种环境下原生的 WebView2 窗口必然崩溃。
func inIsolatedSandbox() bool {
	for _, k := range []string{
		"LSBOX_AUDIT_SHMEM",
		"SANDBOX_CENTER_IPC_ADDRESS",
		"CODEBUDDY_SAFE_DELETE_SANDBOX",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// windowsOptions 组装 Windows 平台选项。
//
// WebviewDisableRendererCodeIntegrity 是这台机器能否显示界面的关键：
// 本机有软件向 msedgewebview2.exe 注入未签名 DLL（代码完整性事件日志中
// tsbx.dll 被拒），WebView2 因此杀掉自己的进程（kind 4/6 → 0）。
func windowsOptions(app *App) *windows.Options {
	crash := "界面进程(WebView2)异常退出。\n\n" +
		"账号池引擎仍在后台运行，自动签到/自动任务不受影响。\n"
	if app.uiURL != "" {
		crash += "\n请用浏览器打开内置控制台继续操作：\n" + app.uiURL
	}
	return &windows.Options{
		WebviewDisableRendererCodeIntegrity: true,
		WebviewGpuIsDisabled:                true,
		// 前端是浅色主题：让标题栏/系统控件也走浅色，否则深色标题栏压在米白页面上很割裂。
		Theme: windows.Light,
		Messages: &windows.Messages{
			WebView2ProcessCrash: crash,
		},
	}
}

// onShutdown 退出清理：停引擎 → 撤托盘。
func (a *App) onShutdown(ctx context.Context) {
	a.shutdown(ctx)
	systray.Quit()
}
