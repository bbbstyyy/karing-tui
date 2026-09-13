# Karing TUI

基于 Go、Bubble Tea 和 sing-box 的终端代理客户端，支持 Linux 与 macOS 的 amd64 / arm64 平台。

通过终端界面管理订阅、节点、代理组、分流规则与 DNS，也可以使用 CLI 在服务器上运行后台代理服务。发布包内置独立的 sing-box 可执行内核及常用规则集，首次运行按需释放到数据目录。

## 功能

- 多订阅与分享链接导入，支持 Clash YAML、sing-box JSON、Base64 节点列表。
- 节点搜索、过滤、测速，以及 Select / URLTest 代理组。
- 域名、正则、IP、地理分类、规则集与逻辑组合分流，内置 geosite / geoip / ACL4SSR 分类库。
- DNS 与 FakeIP 配置、日志、流量状态、路由检测和连通性诊断。
- CLI 后台运行、备份恢复、配置迁移及订阅自动更新。

当前提供 HTTP / SOCKS mixed 入站；不提供 TUN、系统代理切换或 Windows 支持。SSR 节点可解析和展示，但官方 sing-box 内核无法运行，生成配置时会提示处理。

## 安装与首次使用

从 [Releases](https://github.com/bbbstyyy/karing-tui/releases) 下载对应平台的 `tar.gz`，并使用同页的 `checksums.txt` 校验。下面以 Linux amd64 的文件名为例，请替换为实际版本：

```sh
tar -xzf karing-tui-v0.1.0-linux-amd64.tar.gz
./karing version
./karing
```

归档中的 `karing` 是主程序；可将它安装到 PATH 中的目录。后文命令假设已将其命名为 `karing` 并加入 PATH。

1. 按 `2` 打开 Profiles，按 `a` 添加订阅，再按 `u` 更新节点。
2. 按 `3` 查看代理组。首次初始化会创建 Auto / Manual / AI 默认组。
3. 按 `4` 查看或调整分流；按 `7` 设置端口及下载代理。
4. 回到 `1` Dashboard，按 `g` 生成并校验配置，按 `s` 启动核心。
5. 将需要代理的应用指向 `127.0.0.1:2080`（HTTP 或 SOCKS5）。

默认只监听本机，mixed 端口为 `2080`，Clash API 端口为 `9090`。通过 `1`–`7` 切页，`?` 查看帮助，`Ctrl+C` 退出 TUI 并停止由它启动的核心。需要退出终端后持续运行时，使用下方的后台模式。

应用内下载代理在 Settings 的 `download_proxy` 中配置，例如 `http://127.0.0.1:7890`；空值表示直连，应用不会读取系统环境代理变量。订阅还可单独设置代理优先、直连优先或仅使用指定通道。

## CLI 与后台模式

完成配置并退出 TUI 后，可以启动后台服务：

```sh
karing start --json
karing status --json
karing diagnose check https://example.com --json
karing stop --json
```

同一数据目录只允许一个 TUI 或后台服务持有配置写锁。编辑配置、导入备份或执行其他写操作前，先停止后台服务。状态与诊断命令可以在服务运行时使用。

常用命令：

| 命令 | 用途 |
| --- | --- |
| `karing profile list` / `karing profile update` | 查看或更新订阅 |
| `karing proxy list` / `karing proxy select <组> <成员>` | 查看代理组或持久化选择 |
| `karing config check` | 生成并校验 sing-box 配置 |
| `karing core update [版本]` | 更新受管内核，默认采用本次构建的内置版本 |
| `karing ruleset search geosite google` | 搜索内置分类 |
| `karing ruleset update` | 更新规则集缓存 |
| `karing route test example.com` | 离线检查规则匹配，无法判断的规则集会提示 |
| `karing import clash <文件>` / `karing import singbox <文件>` | 合并导入配置 |
| `karing backup export [路径]` / `karing backup import <文件>` | 导出备份或覆盖恢复 |
| `karing help` | 查看全部命令 |

systemd 与 launchd 的配置和安装步骤见 [后台服务说明](docs/services.md)。

## 数据目录

| 平台 | 默认目录 |
| --- | --- |
| Linux | `$XDG_DATA_HOME/karing-tui`，缺省为 `~/.local/share/karing-tui` |
| macOS | `~/Library/Application Support/karing-tui` |

设置 `KARING_HOME` 可使用独立的数据目录，例如：

```sh
KARING_HOME="$PWD/var/demo" karing
```

目录中包含 `karing.db`、`runtime/config.json`、`runtime/bin/sing-box`、缓存、日志及备份。数据库、配置和备份包含订阅地址与节点凭据，分享诊断信息前请移除这些内容。

已有的受管内核优先于发布包内置版本；普通源码构建没有嵌入内核时，还会尝试 PATH 或按需下载。规则集随版本提供离线快照；少量不在快照内的分类和自定义远程规则集仍需要下载。

## IP 规则行为

`ip_cidr` / `geoip` 对纯 IP 连接直接生效。域名连接需要先解析才能匹配 IP 条件，因此 `resolve_ip_rules` 默认关闭。

开启后，经过该解析动作的代理连接会使用解析所得 IP 拨号，代理侧收到 IP 而非原域名；解析失败也会中断连接。内网直连 `private_direct` 默认开启，不需要为它开启域名解析。

## 从源码构建

需要 Go **1.27.0 或更高版本**，版本要求以 [go.mod](go.mod) 为准。使用纯 Go SQLite 驱动，构建不依赖 CGO。

获取源码并构建：

```sh
git clone https://github.com/bbbstyyy/karing-tui.git
cd karing-tui
go build -o bin/karing ./cmd/karing
./bin/karing help
go test -race ./...
```

此构建不包含 sing-box。需要运行代理时可使用 `./bin/karing core update` 获取内核，或在 PATH 中提供 sing-box 1.14.0 及以上版本。

构建包含内核的四平台发布包，需要 Git、tar 和 gzip：

```sh
./scripts/build-release.sh v0.1.0
```

脚本默认获取 sing-box `v1.14.0` 标签，从其源码编译外部内核，再压缩并嵌入每个平台的主程序。产物位于 `dist/karing-tui-v0.1.0/`。同名输出目录已存在时会停止，避免覆盖之前的发布包。

可以复用本地 Git 仓库中的版本标签，或只构建一个目标：

```sh
SING_BOX_SOURCE="$PWD/sing-box" \
  KARING_BUILD_TARGETS="darwin/arm64" \
  ./scripts/build-release.sh dev-local
```

`SING_BOX_VERSION=1.14.0` 可指定不带 `v` 的核心版本号；对应标签必须存在。`SING_BOX_SOURCE` 只用于读取该标签，不会使用本地未提交修改。开发工具下载所需的 `http_proxy` / `https_proxy` 与应用内下载代理分别配置。

## 许可证与来源

Karing TUI 主项目以 **GPL-3.0-or-later** 发布：你可以按照 GNU GPL 第 3 版，或自行选择任何后续版本的条款使用、修改及分发。完整许可见 [LICENSE](LICENSE)。

项目参考 [Karing](https://github.com/KaringX/karing) 的产品设计，使用 [sing-box](https://github.com/SagerNet/sing-box) 作为独立运行的代理核心。第三方资源保留各自版权与许可，详见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。本项目与 KaringX、SagerNet 无官方隶属关系。
