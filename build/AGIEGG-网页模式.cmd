@echo off
rem AGIEGG 网页模式：不启动原生窗口（WebView2），用系统默认浏览器承载界面。
rem 适用于 WebView2 被安全软件/注入干扰而崩溃的机器。
cd /d "%~dp0"
start "" "AGIEGG.exe" --web
