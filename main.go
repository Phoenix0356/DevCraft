// package main 是整个应用的入口包。
// 本应用支持两种构建形态（同一份代码）：
//   - 桌面模式：wails3 build —— 原生窗口（Windows 上基于 WebView2）
//   - 服务器模式：wails3 build -tags server —— 无头 HTTP 服务，浏览器访问
//
// 窗口创建在服务器模式下由框架降级为 no-op，无需条件编译分支。
package main

import (
	"embed" // Go 标准库：把静态文件编译进二进制（≈ 把静态资源打进 jar）
	"log/slog"
	"os"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application" // Wails v3 应用框架
)

// 【编译期指令，勿动】//go:embed 把 frontend/dist（Vue 构建产物：html/js/css）
// 整个打包进可执行文件。"all:" 前缀表示连 . 开头的隐藏文件也包含。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// 全局结构化日志：文本格式输出到 stderr；环境变量 DEVCRAFT_LOG=debug 可开调试级。
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("DEVCRAFT_LOG"), "debug") {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// 创建业务服务对象。依赖组装推迟到 ServiceStartup 生命周期钩子（见 app.go）。
	appService := NewApp()

	// 创建 Wails 应用。Services 里的对象会被反射扫描，自动生成前端绑定
	// （wails3 generate bindings），桌面与浏览器两种环境通用。
	app := application.New(application.Options{
		Name: "DevCraft",
		Services: []application.Service{
			application.NewService(appService),
		},
		Assets: application.AssetOptions{
			// 内嵌前端资源作为 HTTP 资源服务器：桌面模式供 WebView2，
			// 服务器模式供浏览器——两种形态共用同一条资源链路。
			Handler: application.BundledAssetFileServer(assets),
		},
	})

	// 把应用实例注入业务对象（事件/流推送需要它），并注册聊天流处理器。
	// 必须在 app.Run() 之前完成。
	appService.bind(app)

	// 桌面模式：创建原生窗口。服务器模式：该调用被框架降级为 no-op，
	// 每个浏览器标签页连接时自动成为一个 BrowserWindow。
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "DevCraft",
		Width:  1024,
		Height: 768,
		URL:    "/",
	})

	// 进入事件循环，阻塞直到退出（桌面：窗口关闭；服务器：SIGINT/SIGTERM）。
	if err := app.Run(); err != nil {
		slog.Error("Wails 启动失败", "err", err)
		os.Exit(1)
	}
}
