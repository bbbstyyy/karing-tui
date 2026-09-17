// Package cli 提供 karing 的命令行子命令，与 TUI 共用 internal/application 层。
// 约定：`karing`（无参数）进入 TUI；带子命令时以纯文本输出，退出码
// 0 成功、1 操作失败、2 用法错误。
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/catalog"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/core"
	"github.com/bbbstyyy/karing-tui/internal/headless"
	"github.com/bbbstyyy/karing-tui/internal/migrate"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	"github.com/bbbstyyy/karing-tui/internal/routing"
	"github.com/bbbstyyy/karing-tui/internal/storage"
)

// Version 是程序版本号；发布构建经 ldflags 注入（scripts/build-release.sh）。
var Version = "0.1.0-dev"

const usage = `Karing TUI - 基于 sing-box 的终端代理客户端

用法:
  karing                    进入交互式 TUI
  karing <命令> [参数]

命令:
	status                    查看运行状态、订阅与代理组概况
	status --json             以稳定 JSON 输出运行状态
	start [--json]            生成、校验并启动 headless 核心服务
	stop [--json]             请求 headless 服务优雅停止
	restart [--json]          重启 headless 核心服务
  profile list              列出订阅
  profile update [名称...]  更新订阅（缺省为全部启用的订阅）
  proxy list [组名]         列出代理组或组成员
  proxy select <组> <成员>  持久化 select 组的选中节点
  config check              重新生成配置并校验
  core update [版本]        更新 sing-box 内核（缺省为当前内置版本）
  ruleset list              列出自定义规则集与引用中的内置分类
  ruleset search [种类] <词> 搜索内置分类库（种类: geosite/geoip/acl）
	ruleset update            更新全部规则集缓存（自定义 + 引用中的内置分类）
	diagnose                  输出 Clash API 连接/规则诊断
	diagnose check <URL>      通过 mixed 入站检查 HTTP 连通性
  route list                按层列出分流组（层序、层内序号、目标、状态）
  route add <名称> --target <出站> [--kind <层>] <规则集...>
                            新建分流组；--kind 缺省按规则构成推断并回显
  route move <名称> --kind <层> [--pos N]
                            把分流组移到目标层（--pos 缺省追加到层尾）
  route preset <地区> [--merge|--replace] [--yes]
                            对齐地区预置方案（缺省 merge：按名称跳过已存在的组；
                            replace 会删除现有分流组，必须再加 --yes 确认）
  route test <域名|IP>       检测输入会命中哪条分流规则、所属层及出站
  import clash <文件>       从 Clash 配置导入（节点/代理组/分流规则）
  import singbox <文件>     从 sing-box 配置导入（Karing 导出格式同此）
  backup export [路径]      导出备份（缺省 <数据目录>/backups/）
  backup import <文件>      从备份恢复（覆盖当前数据，需无其他实例运行）
  version                   显示版本
  help                      显示本帮助`

// Run 执行 CLI 子命令，返回进程退出码。
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	switch args[0] {
	case "status":
		return withApp(stderr, func(app *application.App) int {
			return cmdStatusArgs(app, args[1:], stdout, stderr)
		})
	case "start":
		return cmdHeadless("start", args[1:], stdout, stderr)
	case "stop":
		return cmdHeadless("stop", args[1:], stdout, stderr)
	case "restart":
		return cmdHeadless("restart", args[1:], stdout, stderr)
	case "diagnose":
		return cmdDiagnose(args[1:], stdout, stderr)
	case "profile":
		return cmdProfile(args[1:], stdout, stderr)
	case "proxy":
		return cmdProxy(args[1:], stdout, stderr)
	case "config":
		return cmdConfig(args[1:], stdout, stderr)
	case "core":
		return cmdCore(args[1:], stdout, stderr)
	case "ruleset":
		return cmdRuleSet(args[1:], stdout, stderr)
	case "route":
		return cmdRoute(args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(args[1:], stdout, stderr)
	case "import":
		return cmdImport(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "karing-tui %s\n", Version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n\n", args[0])
		fmt.Fprintln(stderr, usage)
		return 2
	}
}

func cmdStatusArgs(app *application.App, args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		fmt.Fprintf(stderr, "未知 status 参数 %q\n用法: karing status [--json]\n", arg)
		return 2
	}
	if jsonOut {
		return cmdStatusJSON(app, stdout, stderr)
	}
	return cmdStatus(app, stdout, stderr)
}

type statusJSON struct {
	Version       string                 `json:"version"`
	DataDir       string                 `json:"data_dir"`
	CoreVersion   string                 `json:"core_version,omitempty"`
	Running       bool                   `json:"running"`
	RuntimeState  string                 `json:"runtime_state"`
	SupervisorPID int                    `json:"supervisor_pid,omitempty"`
	CorePID       int                    `json:"core_pid,omitempty"`
	StartedAt     time.Time              `json:"started_at,omitempty"`
	ExitError     string                 `json:"exit_error,omitempty"`
	MixedPort     int                    `json:"mixed_port"`
	ClashAPIPort  int                    `json:"clash_api_port"`
	Config        string                 `json:"config"`
	ConfigExists  bool                   `json:"config_exists"`
	Subscriptions subscriptionStatusJSON `json:"subscriptions"`
	ProxyGroups   []proxyGroupStatusJSON `json:"proxy_groups"`
}

type subscriptionStatusJSON struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
	Nodes   int `json:"nodes"`
}

type proxyGroupStatusJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Members  int    `json:"members"`
	Selected string `json:"selected,omitempty"`
}

func cmdStatusJSON(app *application.App, stdout, stderr io.Writer) int {
	st, err := headless.ReadState(app.Paths)
	if err != nil {
		fmt.Fprintln(stderr, "读取 headless 状态失败:", err)
		return 1
	}
	coreStatus := app.Core.Status()
	subs, err := app.DB.ListSubscriptions()
	if err != nil {
		fmt.Fprintln(stderr, "读取订阅失败:", err)
		return 1
	}
	ss := subscriptionStatusJSON{}
	for _, s := range subs {
		ss.Total++
		if s.Enabled {
			ss.Enabled++
		}
		ss.Nodes += s.NodeCount
	}
	groups, err := app.DB.ListProxyGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取代理组失败:", err)
		return 1
	}
	if len(groups) == 0 {
		groups = statusDefaultGroups()
	}
	gs := make([]proxyGroupStatusJSON, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, proxyGroupStatusJSON{Name: g.Name, Type: g.Type, Members: len(g.Members), Selected: memberDisplayName(app, g.Selected)})
	}
	running := headless.Active(st) || coreStatus.State.String() == "运行中"
	if !running && st.Status != headless.StatusCrashed && detectRunning(app) {
		running = true
		st.Status = headless.StatusRunning
		st.SupervisorPID = 0
		st.CorePID = 0
		st.ExitError = ""
	} else if !running && st.Status == headless.StatusRunning {
		st.Status = headless.StatusCrashed
		if st.ExitError == "" {
			st.ExitError = "supervisor PID 已退出"
		}
	}
	if running && st.Status != headless.StatusCrashed {
		st.Status = headless.StatusRunning
	}
	out := statusJSON{Version: Version, DataDir: app.Paths.Root, CoreVersion: coreStatus.Version,
		Running: running, RuntimeState: st.Status, SupervisorPID: st.SupervisorPID, CorePID: st.CorePID,
		StartedAt: st.StartedAt, ExitError: st.ExitError, MixedPort: app.GetSettings().MixedPort,
		ClashAPIPort: app.GetSettings().ClashAPIPort, Config: app.Paths.Config, ConfigExists: fileExists(app.Paths.Config),
		Subscriptions: ss, ProxyGroups: gs}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintln(stderr, "输出 JSON 失败:", err)
		return 1
	}
	return 0
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }

func cmdHeadless(action string, args []string, stdout, stderr io.Writer) int {
	jsonOut := false
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		fmt.Fprintf(stderr, "未知参数 %q\n用法: karing %s [--json]\n", arg, action)
		return 2
	}
	paths, err := newPaths(stderr)
	if err != nil {
		fmt.Fprintln(stderr, "初始化数据目录失败:", err)
		return 1
	}
	if action == "stop" {
		st, readErr := headless.ReadState(paths)
		if readErr != nil {
			fmt.Fprintln(stderr, readErr)
			return 1
		}
		if st.Status != headless.StatusRunning && st.Status != headless.StatusStarting {
			return headlessResult(action, jsonOut, stdout, "stopped", 0, "")
		}
		if !headless.Active(st) {
			// A supervisor can be killed without getting a chance to persist its
			// terminal state. Repair the stale snapshot so subsequent status and
			// stop calls do not keep treating the dead service as running. The core
			// is normally a child of that supervisor, so clean up an orphan too.
			if err := headless.StopRecordedCore(st); err != nil {
				fmt.Fprintf(stderr, "清理残留 sing-box 失败: %v\n", err)
				return 1
			}
			st.Status = headless.StatusStopped
			st.SupervisorPID, st.CorePID = 0, 0
			st.StoppedAt = time.Now()
			_ = headless.WriteState(paths, st)
			return headlessResult(action, jsonOut, stdout, "stopped", 0, "")
		}
		if err := headless.RequestStop(paths); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			now, _ := headless.ReadState(paths)
			if now.Status == headless.StatusStopped {
				return headlessResult(action, jsonOut, stdout, now.Status, 0, "")
			}
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprintln(stderr, "等待 headless 服务停止超时")
		return 1
	}
	if action == "restart" {
		st, _ := headless.ReadState(paths)
		if headless.Active(st) {
			if err := headless.RequestStop(paths); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				now, _ := headless.ReadState(paths)
				if now.Status == headless.StatusStopped {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	st, _ := headless.ReadState(paths)
	if headless.Active(st) {
		fmt.Fprintln(stderr, "headless 服务已在运行")
		return 1
	}
	pid, err := headless.StartDetached(paths)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		now, _ := headless.ReadState(paths)
		if headless.Active(now) && now.SupervisorPID != pid {
			fmt.Fprintln(stderr, "headless 服务已在运行")
			return 1
		}
		if now.SupervisorPID == pid && now.Status == headless.StatusRunning {
			return headlessResult(action, jsonOut, stdout, now.Status, pid, "")
		}
		if now.SupervisorPID == pid && now.Status == headless.StatusCrashed {
			return headlessResult(action, jsonOut, stdout, now.Status, pid, now.ExitError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintln(stderr, "等待 headless 服务启动超时")
	return 1
}

func headlessResult(action string, jsonOut bool, stdout io.Writer, state string, pid int, errText string) int {
	if jsonOut {
		_ = json.NewEncoder(stdout).Encode(map[string]any{"action": action, "status": state, "supervisor_pid": pid, "error": errText})
	} else if state == headless.StatusCrashed {
		fmt.Fprintf(stdout, "%s 失败: %s\n", action, errText)
	} else if action == "stop" {
		fmt.Fprintln(stdout, "headless 服务已停止")
	} else {
		fmt.Fprintf(stdout, "headless 服务已启动（supervisor PID %d）\n", pid)
	}
	if state == headless.StatusCrashed {
		return 1
	}
	return 0
}

func cmdDiagnose(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return withApp(stderr, func(app *application.App) int {
			client := app.ClashClient()
			if client == nil {
				fmt.Fprintln(stderr, "Clash API 未开启")
				return 1
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stats, conns, err := client.DetailedConnections(ctx)
			if err != nil {
				fmt.Fprintln(stderr, "读取连接失败:", err)
				return 1
			}
			proxies, err := client.Proxies(ctx)
			if err != nil {
				fmt.Fprintln(stderr, "读取代理状态失败:", err)
				return 1
			}
			fmt.Fprintf(stdout, "连接: %d · ↑%d B · ↓%d B\n", stats.Count, stats.UploadTotal, stats.DownloadTotal)
			for _, c := range conns {
				fmt.Fprintf(stdout, "连接 %s: rule=%s outbound=%s chains=%s up=%d down=%d\n", c.ID, c.Rule, c.RulePayload, strings.Join(c.Chains, " / "), c.Upload, c.Download)
			}
			for tag, p := range proxies {
				if p.Type == "selector" || p.Type == "urltest" {
					fmt.Fprintf(stdout, "代理组 %s: %s (%s)\n", tag, p.Now, p.Type)
				}
			}
			return 0
		})
	}
	if args[0] != "check" || len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(stderr, "用法: karing diagnose [check <URL> [--json]]")
		return 2
	}
	jsonOut := len(args) == 3 && args[2] == "--json"
	if len(args) == 3 && !jsonOut {
		fmt.Fprintf(stderr, "未知参数 %q\n", args[2])
		return 2
	}
	return withApp(stderr, func(app *application.App) int {
		u, err := url.Parse(args[1])
		if err != nil || u.Scheme == "" || u.Host == "" {
			fmt.Fprintln(stderr, "URL 非法")
			return 2
		}
		proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", app.GetSettings().MixedPort))
		hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		start := time.Now()
		resp, err := hc.Get(u.String())
		elapsed := time.Since(start).Round(time.Millisecond)
		result := map[string]any{"url": u.String(), "elapsed_ms": elapsed.Milliseconds()}
		if err != nil {
			result["ok"], result["error"] = false, err.Error()
		} else {
			result["ok"], result["status"] = resp.StatusCode < 500, resp.StatusCode
			_ = resp.Body.Close()
		}
		if jsonOut {
			_ = json.NewEncoder(stdout).Encode(result)
		} else if err != nil {
			fmt.Fprintf(stdout, "失败 (%s): %v\n", elapsed, err)
		} else {
			fmt.Fprintf(stdout, "%s %s (%s)\n", resp.Status, u.Host, elapsed)
		}
		if err != nil || resp.StatusCode >= 500 {
			return 1
		}
		return 0
	})
}

// cmdRoute 实现 `karing route list|test|add|move`。
func cmdRoute(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing route <list|test|add|move> [参数]")
		return 2
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "用法: karing route list")
			return 2
		}
		return withApp(stderr, func(app *application.App) int {
			return routeList(app, stdout, stderr)
		})
	case "test":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "用法: karing route test <域名|IP>")
			return 2
		}
		return routeTest(args[1], stdout, stderr)
	case "add":
		return withExclusiveApp(stderr, func(app *application.App) int {
			return routeAdd(app, args[1:], stdout, stderr)
		})
	case "move":
		return withExclusiveApp(stderr, func(app *application.App) int {
			return routeMove(app, args[1:], stdout, stderr)
		})
	case "preset":
		return withExclusiveApp(stderr, func(app *application.App) int {
			return routePreset(app, args[1:], stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n用法: karing route <list|test|add|move> [参数]\n", args[0])
		return 2
	}
}

// routeList 按层列出分流组：层标题给出组数/启用数，组行给出层内序号、目标与规则数。
func routeList(app *application.App, stdout, stderr io.Writer) int {
	groups, err := app.DB.ListRoutingGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取分流组失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "层序（优先级由高到低）: %s\n", strings.Join(config.Kinds, " < "))
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "暂无分流组（karing route add 新建，或 karing route preset cn 导入地区方案）")
		return 0
	}
	for _, layer := range config.GroupByKind(groups) {
		enabled := 0
		for _, g := range layer.Groups {
			if g.Enabled {
				enabled++
			}
		}
		title := fmt.Sprintf("%s（%s，层序 %d）· %d 组 / 启用 %d",
			config.KindLabel(layer.Kind), layer.Kind, config.KindRank(layer.Kind), len(layer.Groups), enabled)
		if layer.Kind == config.KindFinal {
			title += " · 兜底 · 不可选「不处理」"
		}
		fmt.Fprintln(stdout, title)
		if len(layer.Groups) == 0 {
			fmt.Fprintln(stdout, "    （空层）")
			continue
		}
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for i, g := range layer.Groups {
			state := "启用"
			if !g.Enabled {
				state = "停用"
			}
			fmt.Fprintf(w, "    %d\t%s\t%s\t%d 条规则\t%s\n", i+1, g.Name, g.Target, len(g.Rules), state)
		}
		_ = w.Flush()
	}
	return 0
}

// routePreset 对齐地区预置方案（7.4 的显式入口，可重复执行）。
// merge：按名称跳过已存在的组（也是「恢复默认预设」）；replace：先删除现有全部分流组，
// 必须再加 --yes 作为第二次确认——静默替换用户正在运行的分流方案属于高风险行为。
func routePreset(app *application.App, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "用法: karing route preset <地区> [--merge|--replace] [--yes]（地区可选: %s）\n",
			strings.Join(routing.PresetRegions, " / "))
		return 2
	}
	region := args[0]
	modeStr, yes := "", false
	for _, arg := range args[1:] {
		switch arg {
		case "--merge":
			modeStr = "merge"
		case "--replace":
			modeStr = "replace"
		case "--yes":
			yes = true
		default:
			fmt.Fprintf(stderr, "未知参数 %q\n用法: karing route preset <地区> [--merge|--replace] [--yes]\n", arg)
			return 2
		}
	}
	mode, err := routing.ParsePresetMode(modeStr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if mode == routing.PresetReplace && !yes {
		fmt.Fprintln(stderr, "replace 会删除现有的全部分流组（包括你在里面做的修改）。")
		fmt.Fprintf(stderr, "确认请追加 --yes：karing route preset %s --replace --yes\n", region)
		fmt.Fprintln(stderr, "提示: 想保留现有组、只补齐缺失的预置组，用 --merge（缺省）。")
		return 2
	}
	ctx := context.Background()
	report, err := app.Rout.ApplyPreset(region, mode)
	if err != nil {
		fmt.Fprintln(stderr, "对齐失败:", err)
		return 1
	}
	fmt.Fprintln(stdout, "已归入 自定义分流组（custom 层）；层内顺序即优先级。逐条结果:")
	for _, item := range report.Items {
		mark := map[string]string{"新增": "✓", "跳过": "·", "失败": "✗", "偏差": "!", "删除": "−", "补齐": "+"}[item.Action]
		fmt.Fprintf(stdout, "  %s [%s] %s: %s\n", mark, item.Action, item.Name, item.Detail)
	}
	fmt.Fprintln(stdout, report.Summary())
	if code := regenerateAndCheck(app, ctx, stdout, stderr); code != 0 {
		return code
	}
	if report.Failed > 0 {
		return 1
	}
	return 0
}

// regenerateAndCheck 生成并校验配置，顺带把新增引用的规则集缓存补齐（7.4）。
func regenerateAndCheck(app *application.App, ctx context.Context, stdout, stderr io.Writer) int {
	if _, err := app.Bin.Ensure(ctx); err != nil {
		fmt.Fprintf(stderr, "提示: sing-box 二进制不可用（%v），跳过校验\n", err)
		fmt.Fprintln(stdout, "提示: 已写入分流组，稍后执行 karing config check 生成并校验配置")
		return 0
	}
	if err := app.GenerateConfig(ctx); err != nil {
		// application.generateConfig 已经把错误包成「生成配置失败: ...」，
		// 这里不再叠加同样前缀，否则 C15 的 onboarding 文案会变成
		// 「生成配置失败: 生成配置失败: 当前没有可用代理节点…」。
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "配置已生成: %s\n", app.Paths.Config)
	if err := app.CheckConfig(ctx); err != nil {
		fmt.Fprintln(stderr, "校验失败:", err)
		fmt.Fprintln(stdout, "提示: 可能是新增的规则集还没下载，执行 karing ruleset update 后重试")
		return 1
	}
	fmt.Fprintln(stdout, "配置校验通过")
	if probeRunning(app) != "未检测到运行实例" {
		fmt.Fprintln(stdout, "提示: 检测到运行中的实例，需重启后生效（TUI Dashboard 按 r）")
	}
	return 0
}

// routeTest 检测输入命中的规则，并给出所属层——层序是排查优先级问题的关键信息。
func routeTest(input string, stdout, stderr io.Writer) int {
	return withApp(stderr, func(app *application.App) int {
		groups, err := app.DB.ListRoutingGroups()
		if err != nil {
			fmt.Fprintln(stderr, "读取分流规则失败:", err)
			return 1
		}
		result, err := routing.DetectWithPrivateDirect(groups, input, app.GetSettings().PrivateDirect)
		if err != nil {
			fmt.Fprintln(stderr, "检测失败:", err)
			return 1
		}
		fmt.Fprintf(stdout, "输入: %s", result.Normalized)
		if result.IsIP {
			fmt.Fprint(stdout, "（IP）")
		}
		fmt.Fprintln(stdout)
		if result.PrivateDirect {
			fmt.Fprintln(stdout, "结果: 命中内网直连规则 ip_is_private")
			fmt.Fprintln(stdout, "出站: DIRECT")
		} else if !result.Matched {
			fmt.Fprintln(stdout, "结果: 未命中规则")
		} else if result.Fallback {
			fmt.Fprintf(stdout, "结果: 未命中显式规则，使用兜底分流组 %s（层 %s）\n", result.Group.Name, config.KindLabel(result.Kind))
		} else {
			fmt.Fprintf(stdout, "结果: 命中分流组 %s（层 %s，层内第 %d 位）\n",
				result.Group.Name, config.KindLabel(result.Kind), result.Group.Position+1)
			fmt.Fprintf(stdout, "规则: #%d %s", result.RuleIndex+1, result.Rule.Type)
			if result.Rule.Type == "logical" {
				fmt.Fprintf(stdout, "=%s", config.FormatLogicalExpr(result.Rule.Mode, result.Rule.Conditions))
			} else if result.Rule.Value != "" {
				fmt.Fprintf(stdout, "=%s", result.Rule.Value)
			}
			fmt.Fprintln(stdout)
		}
		if result.Matched && !result.PrivateDirect {
			fmt.Fprintf(stdout, "出站: %s\n", result.Target)
		}
		for _, unknown := range result.UnknownRules {
			fmt.Fprintf(stdout, "提示: %s 无法离线判定（规则集内容未展开）\n", unknown)
		}
		return 0
	})
}

// routeAdd 新建分流组。规则集参数合并为**一条** rule_set 规则（多值逗号分隔，语义与
// 多条 rule_set 规则等价），与地区预置的写法保持一致。
func routeAdd(app *application.App, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing route add <名称> --target <出站> [--kind <层>] <规则集...>")
		return 2
	}
	name := args[0]
	target, kind := "", ""
	var ruleSets []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--target" || arg == "--kind":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "%s 缺少取值\n", arg)
				return 2
			}
			i++
			if arg == "--target" {
				target = args[i]
			} else {
				kind = args[i]
			}
		case strings.HasPrefix(arg, "--target="):
			target = strings.TrimPrefix(arg, "--target=")
		case strings.HasPrefix(arg, "--kind="):
			kind = strings.TrimPrefix(arg, "--kind=")
		case strings.HasPrefix(arg, "-"):
			fmt.Fprintf(stderr, "未知参数 %q\n", arg)
			return 2
		default:
			ruleSets = append(ruleSets, arg)
		}
	}
	if strings.TrimSpace(target) == "" {
		fmt.Fprintln(stderr, "缺少 --target（DIRECT / BLOCK / 代理组名）")
		return 2
	}
	explicitKind := kind != ""
	if kind != "" && !config.KindValid(kind) {
		fmt.Fprintf(stderr, "未知分流层 %q（可选: %s）\n", kind, strings.Join(config.Kinds, " / "))
		return 2
	}
	var rules []config.Rule
	if len(ruleSets) > 0 {
		rules = append(rules, config.Rule{Type: "rule_set", Value: strings.Join(ruleSets, ","), Enabled: true})
	}
	suggested := config.SuggestKind(rules)
	if !explicitKind && suggested == config.KindFinal {
		suggested = config.KindCustom
	}
	if !explicitKind {
		kind = suggested
	}
	if config.KindNormalize(kind) == config.KindFinal && len(rules) == 0 {
		// final 层的结构要求：必须且仅有一条 final 规则
		rules = append(rules, config.Rule{Type: "final", Enabled: true})
	}
	g, err := app.Rout.CreateGroupIn(name, target, kind, rules)
	if err != nil {
		fmt.Fprintln(stderr, "创建失败:", err)
		return 1
	}
	if explicitKind {
		fmt.Fprintf(stdout, "已创建分流组 %q → %s（%s 层 · 层序 %d）\n",
			g.Name, g.Target, config.KindLabel(g.Kind), config.KindRank(g.Kind))
	} else {
		fmt.Fprintf(stdout, "已创建分流组 %q → %s（推断层 %s · 层序 %d；如需指定请加 --kind）\n",
			g.Name, g.Target, config.KindLabel(g.Kind), config.KindRank(g.Kind))
	}
	if len(rules) == 0 {
		fmt.Fprintln(stdout, "提示: 未提供规则集，该组暂无规则（TUI Rules 页或再执行一次添加）")
	}
	fmt.Fprintf(stdout, "层内序号: 第 %d 位（层内顺序即优先级）\n", g.Position+1)
	return 0
}

// routeMove 跨层移动：显式改变某个分流组的优先级归属。
func routeMove(app *application.App, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing route move <名称> --kind <层> [--pos N]")
		return 2
	}
	name, kind, pos := args[0], "", -1
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--kind" || arg == "--pos":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "%s 缺少取值\n", arg)
				return 2
			}
			i++
			if arg == "--kind" {
				kind = args[i]
			} else if n, err := strconv.Atoi(args[i]); err == nil {
				pos = n
			} else {
				fmt.Fprintf(stderr, "--pos 须为整数，实得 %q\n", args[i])
				return 2
			}
		case strings.HasPrefix(arg, "--kind="):
			kind = strings.TrimPrefix(arg, "--kind=")
		case strings.HasPrefix(arg, "--pos="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--pos="))
			if err != nil {
				fmt.Fprintf(stderr, "--pos 须为整数，实得 %q\n", arg)
				return 2
			}
			pos = n
		default:
			fmt.Fprintf(stderr, "未知参数 %q\n", arg)
			return 2
		}
	}
	if kind == "" {
		fmt.Fprintln(stderr, "缺少 --kind（目标层）")
		return 2
	}
	groups, err := app.DB.ListRoutingGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取分流组失败:", err)
		return 1
	}
	var target *config.RoutingGroup
	for _, g := range groups {
		if g.Name == name || strings.EqualFold(g.Name, name) {
			target = g
			break
		}
	}
	if target == nil {
		fmt.Fprintf(stderr, "分流组 %q 不存在（karing route list 查看）\n", name)
		return 1
	}
	from := target.Layer()
	if err := app.Rout.MoveGroupToKind(target.ID, kind, pos); err != nil {
		fmt.Fprintln(stderr, "移动失败:", err)
		return 1
	}
	moved, err := app.DB.GetRoutingGroup(target.ID)
	if err != nil {
		fmt.Fprintln(stderr, "回读失败:", err)
		return 1
	}
	where := "层尾"
	if pos >= 0 {
		where = fmt.Sprintf("第 %d 位", moved.Position+1)
	}
	fmt.Fprintf(stdout, "%s: %s 层 → %s 层（%s）\n",
		moved.Name, config.KindLabel(from), config.KindLabel(moved.Layer()), where)
	if from != moved.Layer() {
		fmt.Fprintln(stdout, "提示: 跨层移动改变了该组的优先级归属，需重新生成并应用配置后生效")
	}
	return 0
}

// newPaths 是 CLI 获取数据目录的唯一入口：统一把 platform 的归属告警
// （KARING_HOME 指向了一个与本应用无关的既有非空目录）向 stderr 输出一次。
//
// 让 platform 包自己打印是不行的——它不认识 stdout/stderr 约定；而每个
// 子命令各写一遍也不行——漏掉一处，用户就永远看不到（V5-3 之前
// Paths.RootWarning 全仓库零消费点，赋值了却从没展示过）。
func newPaths(stderr io.Writer) (*platform.Paths, error) {
	paths, err := platform.NewPaths()
	if err != nil {
		return nil, err
	}
	if paths.RootWarning != "" {
		fmt.Fprintf(stderr, "警告: %s\n", paths.RootWarning)
	}
	return paths, nil
}

// withApp 初始化应用并执行 fn；初始化失败时输出错误并返回 1。
func withApp(stderr io.Writer, fn func(*application.App) int) int {
	paths, err := newPaths(stderr)
	if err != nil {
		fmt.Fprintln(stderr, "初始化数据目录失败:", err)
		return 1
	}
	app, err := application.NewReadOnly(paths)
	if err != nil {
		// 「库不存在 / schema 版本不匹配 / 数据目录不可写」是只读命令的预期失败
		// （见 storage.ReadOnlyUnavailable），错误文案本身就是给用户的指引，
		// 直接输出即可，不要套上「初始化应用失败」这种会误导排查方向的前缀。
		if storage.ReadOnlyUnavailable(err) {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stderr, "初始化应用失败:", err)
		return 1
	}
	defer app.Close()
	return fn(app)
}

// withExclusiveApp initializes an application while holding the lifetime
// instance lock. Commands that mutate the database, generated config, or
// caches must use this path so they cannot race with the TUI/headless owner.
func withExclusiveApp(stderr io.Writer, fn func(*application.App) int) int {
	paths, err := newPaths(stderr)
	if err != nil {
		fmt.Fprintln(stderr, "初始化数据目录失败:", err)
		return 1
	}
	app, err := application.NewExclusive(paths)
	if err != nil {
		fmt.Fprintln(stderr, "初始化应用失败:", err)
		return 1
	}
	defer app.Close()
	return fn(app)
}

// --- status ---

func cmdStatus(app *application.App, stdout, stderr io.Writer) int {
	st := app.Core.Status()
	fmt.Fprintf(stdout, "Karing TUI %s\n", Version)
	fmt.Fprintf(stdout, "数据目录: %s\n", app.Paths.Root)
	if st.Version != "" {
		fmt.Fprintf(stdout, "核心: sing-box %s\n", st.Version)
	} else {
		fmt.Fprintln(stdout, "核心: sing-box 未下载（启动时自动获取）")
	}
	fmt.Fprintf(stdout, "运行状态: %s\n", probeRunning(app))

	set := app.GetSettings()
	line := fmt.Sprintf("mixed 端口: %d", set.MixedPort)
	if set.ClashAPIPort > 0 {
		line += fmt.Sprintf("（clash API: %d）", set.ClashAPIPort)
	}
	fmt.Fprintln(stdout, line)

	if fi, err := os.Stat(app.Paths.Config); err == nil {
		fmt.Fprintf(stdout, "配置文件: %s（%s 生成）\n", app.Paths.Config, fi.ModTime().Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintln(stdout, "配置文件: 未生成（karing config check 或 TUI 中按 g）")
	}

	subs, err := app.DB.ListSubscriptions()
	if err != nil {
		fmt.Fprintln(stderr, "读取订阅失败:", err)
		return 1
	}
	enabled, nodes := 0, 0
	for _, s := range subs {
		if s.Enabled {
			enabled++
		}
		nodes += s.NodeCount
	}
	fmt.Fprintf(stdout, "订阅: %d 个（启用 %d），共 %d 节点\n", len(subs), enabled, nodes)

	groups, err := app.DB.ListProxyGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取代理组失败:", err)
		return 1
	}
	if len(groups) == 0 {
		groups = statusDefaultGroups()
	}
	for _, g := range groups {
		fmt.Fprintf(stdout, "代理组: %s [%s]", g.Name, g.Type)
		if g.Selected != "" {
			fmt.Fprintf(stdout, " → %s", memberDisplayName(app, g.Selected))
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

func statusDefaultGroups() []*config.ProxyGroup {
	return []*config.ProxyGroup{
		{Name: storage.DefaultGroupAuto, Type: "urltest", Members: []config.ProxyGroupMember{{Type: "all"}}},
		{Name: storage.DefaultGroupManual, Type: "select", Members: []config.ProxyGroupMember{{Type: "all"}}},
	}
}

// probeRunning 探测是否有 sing-box 实例在运行（可能由 TUI 或其他进程启动）。
func probeRunning(app *application.App) string {
	running, detail := detectRunningDetail(app)
	if running {
		return detail
	}
	return "未检测到运行实例"
}

func detectRunning(app *application.App) bool {
	running, _ := detectRunningDetail(app)
	return running
}

func detectRunningDetail(app *application.App) (bool, string) {
	if c := app.ClashClient(); c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		if _, err := c.Proxies(ctx); err == nil {
			return true, "运行中（clash API 可达）"
		}
	}
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(app.GetSettings().MixedPort)), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		return true, fmt.Sprintf("mixed 端口 %d 监听中", app.GetSettings().MixedPort)
	}
	return false, ""
}

// --- profile ---

func cmdProfile(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing profile <list|update> [参数]")
		return 2
	}
	switch args[0] {
	case "list":
		return withApp(stderr, func(app *application.App) int {
			return profileList(app, stdout, stderr)
		})
	case "update":
		return withExclusiveApp(stderr, func(app *application.App) int {
			return profileUpdate(app, args[1:], stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
		return 2
	}
}

func profileList(app *application.App, stdout, stderr io.Writer) int {
	subs, err := app.DB.ListSubscriptions()
	if err != nil {
		fmt.Fprintln(stderr, "读取订阅失败:", err)
		return 1
	}
	if len(subs) == 0 {
		fmt.Fprintln(stdout, "暂无订阅（在 TUI Profiles 页添加）")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t名称\t状态\t下载通道\t节点数\t最后更新")
	for _, s := range subs {
		state := "启用"
		if !s.Enabled {
			state = "停用"
		}
		updated := "从未更新"
		if !s.LastUpdated.IsZero() {
			updated = s.LastUpdated.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\n", s.ID, s.Name, state, s.DownloadStrategy, s.NodeCount, updated)
	}
	_ = w.Flush()
	return 0
}

func profileUpdate(app *application.App, names []string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	subs, err := app.DB.ListSubscriptions()
	if err != nil {
		fmt.Fprintln(stderr, "读取订阅失败:", err)
		return 1
	}

	var targets []*config.Subscription
	if len(names) == 0 {
		for _, s := range subs {
			if s.Enabled {
				targets = append(targets, s)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintln(stdout, "没有启用的订阅")
			return 0
		}
	} else {
		for _, name := range names {
			s := findSubscription(subs, name)
			if s == nil {
				fmt.Fprintf(stderr, "订阅 %q 不存在\n", name)
				return 1
			}
			targets = append(targets, s)
		}
	}

	failed, updated := 0, 0
	for _, s := range targets {
		res, err := app.Subs.Update(ctx, s.ID)
		if err != nil {
			fmt.Fprintf(stdout, "✗ %s: %v\n", s.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(stdout, "✓ %s: %d 节点\n", res.Name, res.NodeCount)
		updated++
	}

	if updated > 0 {
		if err := app.GenerateConfig(ctx); err != nil {
			fmt.Fprintln(stderr, "重新生成配置失败:", err)
			return 1
		}
		fmt.Fprintln(stdout, "配置已重新生成")
		if probeRunning(app) != "未检测到运行实例" {
			fmt.Fprintln(stdout, "提示: 检测到运行中的实例，需重启后生效（TUI Dashboard 按 r）")
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// findSubscription 按名称查找订阅（先精确匹配，再忽略大小写）。
func findSubscription(subs []*config.Subscription, name string) *config.Subscription {
	for _, s := range subs {
		if s.Name == name {
			return s
		}
	}
	for _, s := range subs {
		if strings.EqualFold(s.Name, name) {
			return s
		}
	}
	return nil
}

// --- proxy ---

func cmdProxy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing proxy <list|select> [参数]")
		return 2
	}
	switch args[0] {
	case "list":
		return withApp(stderr, func(app *application.App) int {
			return proxyList(app, args[1:], stdout, stderr)
		})
	case "select":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "用法: karing proxy select <组名> <成员名>")
			return 2
		}
		return withExclusiveApp(stderr, func(app *application.App) int {
			return proxySelect(app, args[1], args[2], stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
		return 2
	}
}

func proxyList(app *application.App, args []string, stdout, stderr io.Writer) int {
	groups, err := app.DB.ListProxyGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取代理组失败:", err)
		return 1
	}
	if len(args) > 0 {
		g := findGroup(groups, args[0])
		if g == nil {
			fmt.Fprintf(stderr, "代理组 %q 不存在\n", args[0])
			return 1
		}
		kind := map[string]string{"node": "节点", "group": "组", "all": "动态"}
		for _, m := range g.Members {
			marker := " "
			if m.MemberKey() == g.Selected {
				marker = "*"
			}
			line := fmt.Sprintf("%s %s\t%s", marker, memberDisplayName(app, m.MemberKey()), kind[m.Type])
			if m.Type == "node" {
				if n, err := app.DB.GetNode(m.ID); err == nil {
					line += "\t" + latencyText(n.LatencyMS)
				}
			}
			fmt.Fprintln(stdout, line)
		}
		return 0
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "暂无代理组")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "名称\t类型\t成员数\t当前")
	for _, g := range groups {
		cur := "-"
		if g.Selected != "" {
			cur = memberDisplayName(app, g.Selected)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", g.Name, g.Type, len(g.Members), cur)
	}
	_ = w.Flush()
	return 0
}

func proxySelect(app *application.App, groupName, memberName string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	groups, err := app.DB.ListProxyGroups()
	if err != nil {
		fmt.Fprintln(stderr, "读取代理组失败:", err)
		return 1
	}
	g := findGroup(groups, groupName)
	if g == nil {
		fmt.Fprintf(stderr, "代理组 %q 不存在\n", groupName)
		return 1
	}
	key, err := resolveMember(app, g, memberName)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := app.Proxy.SetGroupSelected(g.ID, key); err != nil {
		fmt.Fprintf(stderr, "设置选中项失败: %v\n", err)
		return 1
	}
	if err := app.GenerateConfig(ctx); err != nil {
		fmt.Fprintf(stderr, "重新生成配置失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s → %s（已持久化，配置已重新生成）\n", g.Name, memberDisplayName(app, key))
	if probeRunning(app) != "未检测到运行实例" {
		fmt.Fprintln(stdout, "提示: 检测到运行中的实例，需重启后生效（TUI Dashboard 按 r）")
	}
	return 0
}

// resolveMember 把成员名称解析为成员键。
// 显式成员（节点/嵌套组）按名称匹配；含动态"全部节点"的组可匹配任意启用节点名。
func resolveMember(app *application.App, g *config.ProxyGroup, name string) (string, error) {
	for _, exact := range []bool{true, false} {
		for _, m := range g.Members {
			switch m.Type {
			case "node":
				if n, err := app.DB.GetNode(m.ID); err == nil && nodeUsable(app, n) && matchName(n.Name, name, exact) {
					return m.MemberKey(), nil
				}
			case "group":
				if grp, err := app.DB.GetProxyGroup(m.ID); err == nil && matchName(grp.Name, name, exact) {
					return m.MemberKey(), nil
				}
			case "all":
				nodes, err := app.DB.ListNodes(0)
				if err != nil {
					break
				}
				for _, n := range nodes {
					if nodeUsable(app, n) && matchName(n.Name, name, exact) {
						return "node:" + strconv.FormatInt(n.ID, 10), nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("成员 %q 不在代理组 %q 中（karing proxy list %s 查看成员）", name, g.Name, g.Name)
}

func nodeUsable(app *application.App, n *config.Node) bool {
	if n == nil || !n.Enabled {
		return false
	}
	if n.SubscriptionID == config.ManualSubscriptionID {
		return true
	}
	s, err := app.DB.GetSubscription(n.SubscriptionID)
	return err == nil && s.Enabled
}

// matchName 按精确或忽略大小写比较名称。
func matchName(label, name string, exact bool) bool {
	if exact {
		return label == name
	}
	return strings.EqualFold(label, name)
}

// findGroup 按名称查找代理组（先精确匹配，再忽略大小写）。
func findGroup(groups []*config.ProxyGroup, name string) *config.ProxyGroup {
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	for _, g := range groups {
		if strings.EqualFold(g.Name, name) {
			return g
		}
	}
	return nil
}

// memberDisplayName 把成员键（"node:<id>"/"group:<id>"/"all"）转为可读名称。
func memberDisplayName(app *application.App, key string) string {
	switch {
	case key == "all":
		return "全部节点"
	case strings.HasPrefix(key, "node:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "node:"), 10, 64)
		if n, err := app.DB.GetNode(id); err == nil {
			return n.Name
		}
	case strings.HasPrefix(key, "group:"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(key, "group:"), 10, 64)
		if g, err := app.DB.GetProxyGroup(id); err == nil {
			return g.Name
		}
	}
	return key
}

// latencyText 格式化延迟显示；-1 表示未测速或失败。
func latencyText(ms int64) string {
	if ms < 0 {
		return "-"
	}
	return strconv.FormatInt(ms, 10) + "ms"
}

// --- import ---

func cmdImport(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "用法: karing import <clash|singbox> <配置文件>")
		return 2
	}
	content, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "读取配置文件失败: %v\n", err)
		return 1
	}
	var report migrate.Report
	code := withExclusiveApp(stderr, func(app *application.App) int {
		var err error
		switch args[0] {
		case "clash":
			report, err = migrate.ImportClash(app.DB, string(content))
		case "singbox", "karing":
			report, err = migrate.ImportSingBox(app.DB, string(content))
		default:
			fmt.Fprintf(stderr, "未知格式 %q（支持 clash / singbox）\n", args[0])
			return 2
		}
		if err != nil {
			fmt.Fprintf(stderr, "导入失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "导入完成: %s\n", report.Summary())
		for _, s := range report.Skipped {
			fmt.Fprintf(stdout, "  跳过: %s\n", s)
		}
		// 导入后重新生成配置并校验（失败不影响导入结果，提示用户处理）
		ctx := context.Background()
		if err := app.GenerateConfig(ctx); err != nil {
			fmt.Fprintf(stderr, "提示: 重新生成配置失败: %v\n", err)
			return 0
		}
		if err := app.CheckConfig(ctx); err != nil {
			fmt.Fprintf(stderr, "提示: 配置校验失败: %v\n", err)
			return 0
		}
		fmt.Fprintln(stdout, "配置校验通过")
		return 0
	})
	return code
}

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing backup <export [路径]|import <文件>>")
		return 2
	}
	switch args[0] {
	case "export":
		if len(args) > 2 {
			fmt.Fprintln(stderr, "用法: karing backup export [路径]")
			return 2
		}
		destArg := ""
		if len(args) == 2 {
			destArg = args[1]
		}
		return withExclusiveApp(stderr, func(app *application.App) int {
			dest, err := app.Backup(destArg)
			if err != nil {
				fmt.Fprintf(stderr, "导出备份失败: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "备份已导出: %s\n", dest)
			return 0
		})
	case "import":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "用法: karing backup import <文件>")
			return 2
		}
		return backupImport(args[1], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n", args[0])
		return 2
	}
}

// backupImport 检查无其他实例占用后恢复备份，并重新生成配置校验。
func backupImport(archive string, stdout, stderr io.Writer) int {
	paths, err := newPaths(stderr)
	if err != nil {
		fmt.Fprintln(stderr, "初始化数据目录失败:", err)
		return 1
	}
	// Hold the application lock for the whole restore window. This rejects an
	// active TUI/headless owner and prevents the database from being replaced
	// underneath a live application.
	instanceLock, err := storage.AcquireInstance(paths)
	if err != nil {
		fmt.Fprintf(stderr, "无法恢复: %v（请先退出正在运行的应用）\n", err)
		return 1
	}
	defer instanceLock.Close()
	// Also reserve the headless supervisor namespace so a service cannot start
	// between the ownership check and the restore operation.
	serviceLock, err := headless.Acquire(paths)
	if err != nil {
		fmt.Fprintf(stderr, "无法恢复: %v（请先停止 headless 服务）\n", err)
		return 1
	}
	defer serviceLock.Close()
	// 占用检查 + 校验归档（RestoreArchive 内部会再开库校验迁移）
	probe, err := storage.Open(paths)
	if err != nil {
		fmt.Fprintln(stderr, "打开数据库失败:", err)
		return 1
	}
	if err := probe.EnsureIdle(); err != nil {
		probe.Close()
		fmt.Fprintf(stderr, "无法恢复: %v（若 TUI 正在运行请先退出）\n", err)
		return 1
	}
	probe.Close()

	if err := application.RestoreArchive(paths, archive); err != nil {
		fmt.Fprintf(stderr, "恢复失败: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "备份已恢复，正在重新生成配置…")

	ctx := context.Background()
	code := withApp(stderr, func(app *application.App) int {
		if err := app.GenerateConfig(ctx); err != nil {
			fmt.Fprintf(stderr, "重新生成配置失败: %v\n", err)
			return 1
		}
		if err := app.CheckConfig(ctx); err != nil {
			fmt.Fprintf(stderr, "校验失败: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "配置校验通过，恢复完成")
		return 0
	})
	if code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "提示: 当前数据库已备份为 karing.db.pre-restore")
	return 0
}

// --- config ---

// cmdRuleSet 实现 `karing ruleset list|search|update`。
func cmdRuleSet(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing ruleset <list|search|update>")
		return 2
	}
	switch args[0] {
	case "list":
		return withApp(stderr, func(app *application.App) int {
			return cmdRuleSetList(app, stdout, stderr)
		})
	case "search":
		return cmdRuleSetSearch(args[1:], stdout, stderr)
	case "update":
		return withExclusiveApp(stderr, func(app *application.App) int {
			return cmdRuleSetUpdate(app, stdout, stderr)
		})
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n用法: karing ruleset <list|search|update>\n", args[0])
		return 2
	}
}

// cmdRuleSetList 列出自定义规则集与当前被规则引用的内置分类。
func cmdRuleSetList(app *application.App, stdout, stderr io.Writer) int {
	sets, err := app.DB.ListRuleSets()
	if err != nil {
		fmt.Fprintln(stderr, "读取规则集失败:", err)
		return 1
	}
	refs, err := app.Rules.ReferencedCatalog()
	if err != nil {
		fmt.Fprintln(stderr, "扫描内置分类引用失败:", err)
		return 1
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TAG\t名称\t来源\t格式\t缓存\t状态")
	for _, rs := range sets {
		cached := "未缓存"
		if rs.CachedPath != "" {
			if _, err := os.Stat(rs.CachedPath); err == nil {
				cached = "已缓存"
			}
		}
		state := "启用"
		if !rs.Enabled {
			state = "停用"
		}
		fmt.Fprintf(w, "%s\t%s\t自定义\t%s\t%s\t%s\n", rs.Tag, rs.Name, rs.Format, cached, state)
	}
	for _, ref := range refs {
		cached := "未缓存"
		if app.Rules.CatalogCached(ref) {
			cached = "已缓存"
		}
		fmt.Fprintf(w, "%s\t%s\t内置分类\tsrs\t%s\t引用中\n", ref.Tag(), ref, cached)
	}
	w.Flush()
	if len(sets) == 0 && len(refs) == 0 {
		fmt.Fprintln(stdout, "（暂无规则集；内置分类在规则里直接写 geosite:cn 即可引用）")
	}
	fmt.Fprintf(stdout, "\n内置分类库: geosite %d · geoip %d · acl %d（karing ruleset search 查找）\n",
		catalog.Count(catalog.KindGeosite), catalog.Count(catalog.KindGeoIP), catalog.Count(catalog.KindACL))
	return 0
}

// cmdRuleSetSearch 搜索内置分类库。首个参数为种类时限定该种类。
func cmdRuleSetSearch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "用法: karing ruleset search [geosite|geoip|acl] <关键词>")
		return 2
	}
	kind, query := "", ""
	if len(args) >= 2 && catalog.KindValid(strings.ToLower(args[0])) {
		kind, query = strings.ToLower(args[0]), strings.Join(args[1:], " ")
	} else {
		query = strings.Join(args, " ")
	}
	const limit = 200
	hits := catalog.Search(kind, query, limit+1)
	if len(hits) == 0 {
		fmt.Fprintf(stdout, "无匹配分类（关键词 %q）\n", query)
		return 0
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "引用写法\tTAG\t类型")
	for _, ref := range hits {
		typ := "域名"
		if ref.NeedsResolve() {
			typ = "含 IP 条件"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", ref, ref.Tag(), typ)
	}
	w.Flush()
	if truncated {
		fmt.Fprintf(stdout, "（仅显示前 %d 条，请用更精确的关键词）\n", limit)
	}
	fmt.Fprintln(stdout, "\n在分流规则中填「引用写法」即可（类型选 rule_set），无需预先添加。")
	return 0
}

// cmdRuleSetUpdate 更新全部规则集缓存：自定义远程规则集 + 引用中的内置分类。
func cmdRuleSetUpdate(app *application.App, stdout, stderr io.Writer) int {
	ctx := context.Background()
	var failed bool
	if err := app.Rules.DownloadAll(ctx); err != nil {
		fmt.Fprintln(stderr, "更新自定义规则集失败:", err)
		failed = true
	}
	refs, err := app.Rules.ReferencedCatalog()
	if err != nil {
		fmt.Fprintln(stderr, "扫描内置分类引用失败:", err)
		return 1
	}
	for _, ref := range refs {
		if err := app.Rules.DownloadCatalog(ctx, ref); err != nil {
			fmt.Fprintf(stderr, "更新 %s 失败: %v\n", ref, err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "%s 已更新\n", ref)
	}
	if failed {
		return 1
	}
	fmt.Fprintf(stdout, "规则集缓存已更新（内置分类 %d 个）\n", len(refs))
	return 0
}

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(stderr, "用法: karing config check")
		return 2
	}
	return withExclusiveApp(stderr, func(app *application.App) int {
		ctx := context.Background()
		if _, err := app.Bin.Ensure(ctx); err != nil {
			fmt.Fprintf(stderr, "sing-box 二进制不可用: %v\n", err)
			return 1
		}
		if err := app.GenerateConfig(ctx); err != nil {
			// 同上：application.generateConfig 已带「生成配置失败:」前缀。
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "配置已生成: %s\n", app.Paths.Config)
		if err := app.CheckConfig(ctx); err != nil {
			fmt.Fprintf(stderr, "校验失败: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "校验通过")
		return 0
	})
}

func cmdCore(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "update" || len(args) > 2 {
		fmt.Fprintln(stderr, "用法: karing core update [版本]")
		return 2
	}
	version := core.EmbeddedVersion
	if len(args) == 2 {
		version = args[1]
	}
	return withExclusiveApp(stderr, func(app *application.App) int {
		if app.Core.IsRunning() {
			fmt.Fprintln(stderr, "sing-box 正在运行，请先停止后再更新内核")
			return 1
		}
		if err := app.Bin.Download(context.Background(), version); err != nil {
			fmt.Fprintf(stderr, "更新 sing-box 内核失败: %v\n", err)
			return 1
		}
		got, err := app.Bin.Version(context.Background())
		if err != nil {
			fmt.Fprintf(stderr, "更新后读取 sing-box 版本失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "sing-box 内核已更新: %s\n", got)
		return 0
	})
}
