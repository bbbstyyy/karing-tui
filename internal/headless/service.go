package headless

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

// StartDetached 启动当前可执行文件的 supervisor 子进程，并返回其 PID。
func StartDetached(paths *platform.Paths) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("定位 karing 可执行文件失败: %w", err)
	}
	ClearStop(paths)
	p, err := spawnDetached(exe, []string{"_service"}, paths.Logs+string(os.PathSeparator)+"karing-headless.log")
	if err != nil {
		return 0, fmt.Errorf("启动 headless supervisor 失败: %w", err)
	}
	return p.Pid, nil
}

// RunService 是隐藏的 supervisor 入口。返回值可直接作为进程退出码。
func RunService(paths *platform.Paths) int {
	lock, err := Acquire(paths)
	if err != nil {
		// 另一个 supervisor 已持有锁时，不能覆盖其正在运行的状态。
		// 调用方可通过现有状态和退出码判断本次启动未获得所有权。
		return 1
	}
	defer lock.Close()
	started := time.Now()
	st := State{Status: StatusStarting, SupervisorPID: os.Getpid(), StartedAt: started, Config: paths.Config}
	_ = WriteState(paths, st)

	app, err := application.NewExclusive(paths)
	if err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	defer app.Close()
	if err := app.GenerateConfig(context.Background()); err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, "生成配置失败: "+err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	if err := app.StartCore(context.Background()); err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	coreStatus := app.Core.Status()
	st.Status, st.CorePID, st.Version, st.MixedPort = StatusRunning, app.Core.PID(), coreStatus.Version, app.GetSettings().MixedPort
	_ = WriteState(paths, st)

	ctx, cancel := context.WithCancel(context.Background())
	autoDone := make(chan struct{})
	go func() {
		defer close(autoDone)
		app.StartAutoUpdate(ctx)
	}()
	// Stop the updater before app.Close closes its database and log writer.
	// AutoUpdateOnce can be in the middle of a download, database write, or
	// config generation when the supervisor receives a stop request.
	defer func() {
		cancel()
		<-autoDone
		_ = app.StopCore()
	}()

	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopSignals)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopSignals:
			_ = app.StopCore()
			st.Status, st.StoppedAt, st.CorePID = StatusStopped, time.Now(), 0
			_ = WriteState(paths, st)
			return 0
		case <-ticker.C:
			if consumeStop(paths) {
				_ = app.StopCore()
				st.Status, st.StoppedAt, st.CorePID = StatusStopped, time.Now(), 0
				_ = WriteState(paths, st)
				return 0
			}
			if !app.Core.IsRunning() {
				st.Status, st.StoppedAt, st.CorePID = StatusCrashed, time.Now(), 0
				if st.ExitError == "" {
					st.ExitError = "sing-box 运行期间退出"
				}
				_ = WriteState(paths, st)
				return 1
			}
		}
	}
}
