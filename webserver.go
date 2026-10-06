package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// uiBasePort 是内置界面的起始端口；被占用时向上顺延 16 个。
//
// 存在这条通路的原因：Wails 在 WebView2 浏览器进程退出时会直接 os.Exit(-1)
// （internal/frontend/desktop/windows/frontend.go:539），无法用 recover 拦住。
// 本机装有会向浏览器进程注入未签名 DLL 的软件（代码完整性事件可见
// tsbx.dll 被拒），所以必须留一条不经过 WebView2 的界面通路。
const uiBasePort = 17863

// uiCallReq 是浏览器模式下前端调用后端方法的请求体。
type uiCallReq struct {
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}

type uiCallResp struct {
	OK     bool        `json:"ok"`
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

// dispatch 按方法名调用 App 上同名的绑定方法（参数按 JSON 解包）。
// 与 Wails 的 JS 绑定保持一一对应，前端两条通路可以共用同一套调用名。
func (a *App) dispatch(method string, raw []json.RawMessage) (interface{}, error) {
	str := func(i int) string {
		if i >= len(raw) {
			return ""
		}
		var s string
		if json.Unmarshal(raw[i], &s) == nil {
			return s
		}
		return ""
	}
	boolean := func(i int) bool {
		if i >= len(raw) {
			return false
		}
		var b bool
		if json.Unmarshal(raw[i], &b) == nil {
			return b
		}
		return false
	}

	switch method {
	case "Status":
		return a.Status(), nil
	case "Start":
		return nil, a.Start(str(0))
	case "Stop":
		return nil, a.Stop(str(0))
	case "Restart":
		return nil, a.Restart(str(0))
	case "Signin":
		return a.Signin(str(0)), nil
	case "SigninAll":
		return a.SigninAll(), nil
	case "Credits":
		return a.Credits(str(0)), nil
	case "Accounts":
		return a.Accounts(str(0)), nil
	case "PanelInfo":
		return a.PanelInfo(), nil
	case "OpenPanel":
		return nil, a.OpenPanel(str(0))
	case "OpenWebUI":
		return nil, a.OpenWebUI()
	case "OpenFolder":
		return nil, a.OpenFolder()
	case "IsAutostart":
		return a.IsAutostart(), nil
	case "SetAutostart":
		return nil, a.SetAutostart(boolean(0))
	case "QuitApp":
		// 先回响应，再异步退出。
		go func() {
			time.Sleep(250 * time.Millisecond)
			a.shutdown(a.ctx)
			os.Exit(0)
		}()
		return nil, nil
	}
	return nil, fmt.Errorf("未知方法: %s", method)
}

// handleUICall 是 /api/call 的分发入口（也挂在 Wails AssetServer 的 Handler 上）。
// 安全：写操作要求自定义头 X-AGIEGG，跨站请求会因预检失败被浏览器拦下。
func (a *App) handleUICall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost || r.Header.Get("X-AGIEGG") != "1" {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error":"forbidden"}`))
		return
	}
	var req uiCallReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		_, _ = w.Write([]byte(`{"ok":false,"error":"请求体解析失败"}`))
		return
	}
	res, err := a.dispatch(req.Method, req.Args)
	out := uiCallResp{OK: err == nil, Result: res}
	if err != nil {
		out.Error = err.Error()
	}
	_ = json.NewEncoder(w).Encode(out)
}

// startUIServer 在 127.0.0.1 上起一个轻量 HTTP 服务，既作为 Wails 窗口的备份界面，
// 也支撑 --web 模式（完全不依赖 WebView2）。
//
// 安全：只绑本地回环；写操作要求自定义头 X-AGIEGG，跨站请求会因预检失败被浏览器拦下。
func (a *App) startUIServer(ui fs.FS) error {
	if a.uiSrv != nil {
		return nil
	}
	if ui == nil {
		return fmt.Errorf("界面资源未加载")
	}

	mux := a.buildRootHandler(ui)

	for port := uiBasePort; port < uiBasePort+16; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		a.uiSrv = srv
		a.uiPort = port
		a.uiURL = "http://" + addr + "/"
		go func() { _ = srv.Serve(ln) }()
		return nil
	}
	return fmt.Errorf("界面端口 %d-%d 全部被占用", uiBasePort, uiBasePort+15)
}

// openBrowser 用系统默认浏览器打开地址（rundll32 方式不弹控制台窗口）。
func openBrowser(url string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// openBrowserBestEffort 打开浏览器，失败不影响主流程。
func (a *App) openBrowserBestEffort() {
	url := a.uiURL
	if url == "" {
		if a.uiPort == 0 {
			return
		}
		url = fmt.Sprintf("http://127.0.0.1:%d/", a.uiPort)
	}
	_ = openBrowser(url)
}
