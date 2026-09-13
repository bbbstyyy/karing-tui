package main

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/cli"
	"github.com/bbbstyyy/karing-tui/internal/headless"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/tui"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "_service" {
		paths, err := platform.NewPaths()
		if err != nil {
			fmt.Fprintln(os.Stderr, "初始化数据目录失败:", err)
			os.Exit(1)
		}
		os.Exit(headless.RunService(paths))
	}
	// 带子命令时走 CLI（status/profile/proxy/config...），无参数进入 TUI。
	if len(os.Args) > 1 {
		os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
	}

	paths, err := platform.NewPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化数据目录失败:", err)
		os.Exit(1)
	}

	app, err := application.NewExclusive(paths)
	if err != nil {
		fmt.Fprintln(os.Stderr, "初始化应用失败:", err)
		os.Exit(1)
	}
	defer app.Close()

	// 启动时生成一次配置，保证 Dashboard 有配置状态可展示。
	if err := app.GenerateConfig(context.Background()); err != nil {
		app.AppLog.AppendLine("生成配置失败: " + err.Error())
	}

	// 订阅自动更新（间隔为 0 时立即返回）。
	autoCtx, cancelAuto := context.WithCancel(context.Background())
	autoDone := make(chan struct{})
	go func() {
		defer close(autoDone)
		app.StartAutoUpdate(autoCtx)
	}()

	program := tea.NewProgram(tui.NewRoot(app), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		cancelAuto()
		<-autoDone
		fmt.Fprintln(os.Stderr, "TUI 运行失败:", err)
		os.Exit(1)
	}
	// 先停止并等待自动更新循环，确保它不再访问 DB，再由 defer 关闭应用。
	cancelAuto()
	<-autoDone
}
