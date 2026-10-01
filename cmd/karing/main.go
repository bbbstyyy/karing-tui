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

	if err := runTUI(func(app *application.App) error {
		_, err := tea.NewProgram(tui.NewRoot(app), tea.WithAltScreen()).Run()
		return err
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Return through deferred cleanup before main exits, including terminal errors.
func runTUI(run func(*application.App) error) error {
	paths, err := platform.NewPaths()
	if err != nil {
		return fmt.Errorf("初始化数据目录失败: %w", err)
	}
	app, err := application.NewExclusive(paths)
	if err != nil {
		return fmt.Errorf("初始化应用失败: %w", err)
	}
	defer app.Close()
	if paths.RootWarning != "" {
		app.AppLog.AppendLine("警告: " + paths.RootWarning)
	}
	if err := app.GenerateConfig(context.Background()); err != nil {
		app.AppLog.AppendLine(err.Error())
	}
	autoCtx, cancelAuto := context.WithCancel(context.Background())
	autoDone := make(chan struct{})
	go func() {
		defer close(autoDone)
		app.StartAutoUpdate(autoCtx)
	}()
	defer func() {
		cancelAuto()
		<-autoDone
	}()
	if err := run(app); err != nil {
		return fmt.Errorf("TUI 运行失败: %w", err)
	}
	return nil
}
