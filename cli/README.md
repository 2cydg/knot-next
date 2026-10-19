# Knot CLI foundation

独立 module `knot-cli`，工具链为 Go 1.27.1。前端通过 core 的公开 HTTP/WS API 工作；当前已实现入口与中英文基础，管理页面和真实 SSH/SFTP 尚未实现。非信息入口会明确报错并以 1 退出。

```bash
cd cli
GOWORK=off go test ./...
GOWORK=off go build -o ../bin/knot .
../bin/knot --help
../bin/knot --version
```

根目录 `scripts/build.sh`、`scripts/test.sh`、`scripts/lint.sh` 均依次处理 core 和 cli；`scripts/build.sh --all` 构建 linux/darwin/windows × amd64/arm64，CGO=0，Windows 带 `.exe`。两个 module 不互相 import；UI 库在实现根 TUI 时引入并固定为 Bubble Tea v2、Bubbles v2、Lip Gloss v2。

## 入口契约

| 命令 | 意图 | SSH 结束后 |
| --- | --- | --- |
| `knot` | 管理 TUI，服务器页 | 回服务器页 |
| `knot server` / `knot servers` | 服务器页 | 回服务器页 |
| `knot key` / `knot keys` | 密钥页 | 回服务器页 |
| `knot proxy` / `knot proxies` | 代理页 | 回服务器页 |
| `knot sync` | 同步服务配置页 | 回服务器页，不执行同步 |
| `knot <alias>` / `knot ssh <alias>` | 别名直连 | 退出程序 |
| `knot --help` / `knot --version` | 本地信息 | 直接退出 |

精确英文页面关键字优先：`knot key` 打开页面，`knot ssh key` 直连同名别名。单个其他位置参数交给后续 core resolve，不在本地判断别名是否存在；settings/status/files 不注册页面。多余参数、空参数、未知 flag、缺少 ssh alias 返回 2。没有 `--json`、旧 Cobra 命令树或补全。

`LaunchContext` 的 mode、initial_page、server_ref、return_policy 通过只读方法访问，controller 必须传递其退出策略。server_ref 此时仅是未解析别名；真实连接必须采用 core resolve 返回的持久服务器 ID。

help/version 不检测终端、不读取启动目录、不发现 core。其余入口要求 stdin/stdout 同时为 TTY。前端不会自动启动 core，也不会读写业务配置或偏好文件；真实 runtime/token 发现将在 API 阶段接入。届时需要用户先手动运行 `knot-core`。

退出契约：远端 exit-status 0–255 原样保留，缺失/越界状态返回 1；本地连接/协议失败 1，参数错误 2，主动取消 130。管理 TUI 保留单次 SSH 结果继续运行。当前提供解析与状态转换边界，实际远端状态和交接验证属于 SSH 阶段。

## 语言与默认值

启动只检测一次语言：首选语言主标签为 zh 则中文，其他或查询失败则英文，不向后寻找支持的中文。命令、别名、路径、指纹、PTY bytes 不翻译。中英文 catalog 编译内置，未知 key 与缺少模板参数有显式诊断和通用本地化回退。

- Linux/Unix：LC_ALL → LC_MESSAGES → LANG；非空且非 C/POSIX（包括 C.UTF-8）时，LANGUAGE 的第一个非空项可覆盖。空 locale 忽略 LANGUAGE。
- Windows：`GetUserPreferredUILanguages(MUI_LANGUAGE_NAME)` 查询当前用户；失败回退英文，不按区域或代码页推断。
- macOS：`/usr/bin/defaults read -g AppleLanguages`，固定参数不经过 shell，1 秒期限、16 KiB 输出上限，无 CGO；失败按上述 locale 环境规则/英文回退。原生 macOS/Windows 查询尚待执行，交叉构建只证明编译。

固定默认值为 forward_agent=true、连接时清屏和前端启动目录。core 自身 idle/keepalive/log/recent 等默认值不由前端重写。

阶段状态、验证和续接说明见工作区根目录的 [开发进度](../../docs/cli-tui-progress.md)。
