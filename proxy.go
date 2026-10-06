package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// 引擎反代：把 wb2api 镜像到本程序界面的同一源下，管理面板因此能嵌进自家窗口。
//
// 必须反代而不是直接 iframe：wb2api 面板响应带 X-Frame-Options: DENY 和
// CSP frame-ancestors 'none'（实测响应头），跨源嵌入必被浏览器拦下。
// 经这里转发时剥掉这三个头，面板页面、app.js、/panel/api/* 全部原样通过。
// 面板的所有请求都在 /panel/ 前缀下（app.js 实测确认，无 WebSocket），
// 但这里按「界面资源未命中即代理」的兜底策略转发，面板以后加路径也不用改代码。

// engineProxy 返回指向 wb2api 的反向代理。端口取实时配置，引擎重启不失效。
func (a *App) engineProxy() http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			tgt := &url.URL{Scheme: "http", Host: a.wbListen()}
			pr.SetURL(tgt)
			pr.Out.Host = tgt.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			// X-Frame-Options / CSP frame-ancestors 挡 iframe；COOP 顺手清掉，
			// 避免它对窗口引用关系产生意外影响。
			resp.Header.Del("X-Frame-Options")
			resp.Header.Del("Content-Security-Policy")
			resp.Header.Del("Cross-Origin-Opener-Policy")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="zh-CN"><meta charset="utf-8">
<title>引擎未运行</title><body style="font-family:system-ui,'Segoe UI','Microsoft YaHei',sans-serif;background:#f6f8fa;color:#59636e;display:flex;align-items:center;justify-content:center;height:96vh;margin:0">
<div style="text-align:center"><div style="font-size:15px;color:#1f2328;margin-bottom:6px">引擎未运行</div>
<div style="font-size:12.5px">管理面板依赖 wb2api 引擎，请回到启动器点击「启动」。</div></div></body>`)
		},
	}
}

// wbListen 返回引擎监听地址（形如 127.0.0.1:7863）。
func (a *App) wbListen() string {
	if a.wb != nil && a.wb.ListenAddr != "" {
		return a.wb.ListenAddr
	}
	return "127.0.0.1:9" // 引擎未构建：端口丢弃，由 ErrorHandler 出 502 提示页
}

// buildRootHandler 统一入口：内置 HTTP 服务与 Wails AssetServer 共用同一套路由，
// 两条通路（原生窗口 / 浏览器）的面板行为因此完全一致。
func (a *App) buildRootHandler(ui fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/call", a.handleUICall)
	mux.Handle("/", a.uiOrProxy(ui))
	return mux
}

// uiOrProxy 界面静态文件优先，未命中的一律反代给引擎。
// 静态资源目前只有 index.html 与 egg.png；每次先在 ui FS 里确认，
// 以后新增资源无需改这里。
func (a *App) uiOrProxy(ui fs.FS) http.Handler {
	files := http.FileServer(http.FS(ui))
	px := a.engineProxy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" { // FileServer 自行处理 / → index.html
			files.ServeHTTP(w, r)
			return
		}
		if f, err := ui.Open(strings.TrimSuffix(p, "/")); err == nil {
			f.Close()
			files.ServeHTTP(w, r)
			return
		}
		px.ServeHTTP(w, r)
	})
}
