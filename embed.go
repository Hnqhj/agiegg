package main

import "embed"

// 便携包：引擎二进制 + config.json + auths 账号 + 维护工具（go:embed 打进单个 exe）
//
// 只嵌 wb：Trae 池已下线，embedded/trae 的 26MB 不再打进 exe
// （也不再解包、不再拉起 tw2api）。
//
//go:embed embedded/agiegg.env embedded/icon.ico embedded/icon.png
//go:embed all:embedded/wb
var embeddedFS embed.FS

// 前端 UI（纯 HTML/CSS/JS，无构建步骤）
//
//go:embed all:frontend/dist
var uiFS embed.FS

// 托盘图标：必须是 ICO 字节。systray 会把它写入无扩展名的临时文件再交给
// Windows LoadImage，LoadImage 只认 ICO/BMP，传 PNG 会加载失败。
//
//go:embed embedded/icon.ico
var iconICO []byte
