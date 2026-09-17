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
	// 拿到自己的身份才写状态。Acquire 已经要求过一次 processIdentity，所以这里
	// 失败属于异常；宁可记 crashed 也不要写下一份以后必然被判为 unverifiable、
	// 把用户锁在「需手工清理」里的状态。
	self, err := processIdentity(os.Getpid())
	if err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	st.SupervisorStartToken = self.Token
	_ = WriteState(paths, st)

	app, err := application.NewExclusive(paths)
	if err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	defer app.Close()
	if err := app.GenerateConfig(context.Background()); err != nil {
		// err 已带「生成配置失败:」前缀（application.generateConfig），不再叠加。
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	if err := app.StartCore(context.Background()); err != nil {
		st.Status, st.ExitError, st.StoppedAt = StatusCrashed, err.Error(), time.Now()
		_ = WriteState(paths, st)
		return 1
	}
	coreStatus := app.Core.Status()
	st.Status, st.CorePID = StatusRunning, app.Core.PID()
	st.Version, st.MixedPort = coreStatus.Version, app.GetSettings().MixedPort
	// core 的可验证身份与 PGID 必须在 core 刚启动、leader 一定还活着的时候
	// 立刻记录：supervisor 一旦被杀，这两个字段就是「这个进程组属于旧 core」
	// 的唯一证据，事后无法再推导。
	id, idErr := processIdentity(st.CorePID)
	if idErr != nil {
		// 刚才还能读到自己（Acquire 里），此刻却读不到刚启动的 child：拒绝
		// 继续。让一个无法验证、因而永远无法自动清理的 core 跑下去，比启动
		// 失败更糟——残留 core 会一直占着端口。
		_ = app.StopCore()
		st.Status, st.ExitError, st.StoppedAt, st.CorePID = StatusCrashed,
			"无法读取 sing-box 进程身份: "+idErr.Error(), time.Now(), 0
		_ = WriteState(paths, st)
		return 1
	}
	st.CoreStartToken = id.Token
	st.CorePGID = st.CorePID
	if pgid, err := processGroupID(st.CorePID); err == nil {
		st.CorePGID = pgid
	}
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
			markStopped(&st)
			_ = WriteState(paths, st)
			return 0
		case <-ticker.C:
			if consumeStop(paths) {
				_ = app.StopCore()
				markStopped(&st)
				_ = WriteState(paths, st)
				return 0
			}
			if !app.Core.IsRunning() {
				markCoreGone(&st)
				st.Status, st.StoppedAt = StatusCrashed, time.Now()
				if st.ExitError == "" {
					st.ExitError = "sing-box 运行期间退出"
				}
				_ = WriteState(paths, st)
				return 1
			}
		}
	}
}
