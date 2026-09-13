# 后台服务

先使用 TUI 完成配置、退出 TUI，再选择命令行后台模式或系统服务。两种方式使用同一数据目录时不能同时运行。以下模板假定可执行文件位于 `/usr/local/bin/karing`，请按实际安装位置调整。

## 命令行后台模式

```sh
karing start
karing status --json
karing restart
karing stop
```

`start` 会启动 supervisor 子进程后返回。TUI、后台服务和 CLI 写操作共享单实例锁；修改配置或恢复备份前先停止服务。

## Linux：systemd 用户服务

[karing-headless.service](karing-headless.service) 使用前台 supervisor 入口 `_service`，让 systemd 持续跟踪主进程。`start` 命令会自行转到后台，因此不能作为 `Type=simple` 的入口。

```sh
mkdir -p "$HOME/.config/systemd/user"
cp docs/karing-headless.service "$HOME/.config/systemd/user/karing-headless.service"
systemctl --user daemon-reload
systemctl --user enable --now karing-headless.service
systemctl --user status karing-headless.service
```

模板明确将数据目录设为 `~/.local/share/karing-tui`。如果 TUI 使用了 `XDG_DATA_HOME` 或 `KARING_HOME`，请同步调整服务的 `Environment=KARING_HOME=...`。

查看日志和停止服务：

```sh
journalctl --user -u karing-headless.service -f
systemctl --user stop karing-headless.service
```

用户服务的登录/退出生命周期由系统的 user manager 配置决定。需要无登录会话运行时，可按服务器管理策略配置 lingering。

## macOS：launchd 用户服务

[karing-headless.plist](karing-headless.plist) 同样直接运行 `_service`，启动时运行并在异常退出后重启。默认使用 `~/Library/Application Support/karing-tui`。

```sh
mkdir -p "$HOME/Library/LaunchAgents"
cp docs/karing-headless.plist "$HOME/Library/LaunchAgents/com.karing.tui.headless.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.karing.tui.headless.plist"
launchctl print "gui/$(id -u)/com.karing.tui.headless"
```

需要自定义数据目录时，在 plist 中添加 `EnvironmentVariables` 字典并设置 `KARING_HOME` 的绝对路径；launchd 不会展开路径中的 `~` 或 shell 变量。

停止并卸载当前会话中的服务：

```sh
launchctl bootout "gui/$(id -u)/com.karing.tui.headless"
```

使用系统服务时，通过 `systemctl` / `launchctl` 管理生命周期。状态和日志仍可通过 `karing status --json` 及数据目录中的 `logs/` 查看。
