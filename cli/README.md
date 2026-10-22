# Knot CLI / TUI

独立 module `knot-cli`，工具链为 Go 1.27.1。前端通过 core 的公开 HTTP/WS API 工作，已实现中英文根 TUI、真实资源列表、搜索、详情、离线/认证错误及事件恢复。新增、编辑、删除、代理测试、同步密码写入和真实 SSH/SFTP 留到后续阶段，按键明确提示开发中，不提交模拟结果。

```bash
cd cli
GOWORK=off go test ./...
GOWORK=off go build -o ../bin/knot .
../bin/knot --help
../bin/knot --version
```

根目录 `scripts/build.sh`、`scripts/test.sh`、`scripts/lint.sh` 均依次处理 core 和 cli；`scripts/build.sh --all` 构建 linux/darwin/windows × amd64/arm64，CGO=0，Windows 带 `.exe`。两个 module 不互相 import。固定 Bubble Tea v2.1.0、Bubbles v2.2.1、Lip Gloss v2.0.6，WS 使用 coder/websocket v1.8.15；运行依赖许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 启动与发现

先在另一个终端手动启动 `knot-core`，再运行 `knot` 或 `knot key/proxy/sync`。默认读取 `$XDG_STATE_HOME/knot/runtime/core.json`；未设置 XDG_STATE_HOME 时，三平台均与 core 一致使用用户目录 `.local/state/knot/runtime/core.json`。可用绝对路径环境变量 `KNOT_RUNTIME_FILE` 指定实际 runtime 文件；前端只读此文件及其中 `token_path`，不读取或改写业务配置。

用户目录查询失败时与 core 一致回退到当前用户资料中的 HomeDir。相对 XDG_STATE_HOME 按 CLI 启动目录解析；core 发布相对 token 路径时，仅接受其 runtime_path 与 token_path 指向同目录 `core.json` / `token` 的布局，并按实际发现文件目录定位 token，因此显式 runtime 路径也可访问不同工作目录启动的 core。

仅接受本机 loopback 地址，支持 core 实际 `host:port` 和 IPv6；认证头不经过系统 HTTP 代理或重定向。先从文件 bootstrap，再调用已认证 `/v1/runtime`、version、health、capabilities、config。读取前后核对 instance_id，服务重启/换地址/换 token 使旧资源状态失效。能力按 available 判定，HTTP 200 不等于 Agent/crypto 全部可用。

离线、认证失败和协议错误有独立状态，进入操作区按 r 重试；不自动启动服务。事件断流以 GET 和重新订阅恢复，根模型每 4 秒复核实际状态；请求最长 15 秒，旧页面/请求结果不会覆盖新状态。完整响应重新创建 DTO，省略字段清空旧关联。

## 原型交互

默认入口聚焦左菜单；页面直达聚焦右操作区。左菜单 ↑↓ 循环切页，→/Enter/Tab 进入操作区；业务字母键在菜单无效。操作区 ←/Esc/Tab 返回菜单，↑↓/Home/End 选行，/ 搜索，r 刷新，i 查看独立详情。密钥/代理/同步列表 Enter 打开详情，服务器 c/Enter 的 SSH 尚在开发中。

服务器默认按 core 的 last_used 排序，仅别名子串搜索；输入框内 / 在开头切换标签子串搜索，Esc/Enter 保留查询并回列表。刷新按资源 ID 保留选择。同步密码未设置时显示原型门槛，仅保留 s 的业务入口及通用导航。

? 打开帮助，Esc/Enter 返回，↑↓ 滚动；Ctrl+c 在菜单、操作区、搜索及普通模态中退出管理 TUI，外部 SIGINT/SIGTERM 同样按取消退出 130。统一表单组件支持 Tab/Enter 下一字段及末项提交、Ctrl+s、Shift+Enter 换行、多行粘贴、必填定位、忙状态与 Y/N/Esc 草稿路由。主机信任/传输/关闭 SFTP 专用模态只接受 Enter/Esc，选择器每页 5 项且翻页不穿透；这些组件的真实业务使用由后续阶段接入。所有业务确认均检查模态所属 core 实例，重启后保持旧框供取消、禁止用旧 ID 提交到新实例。

后台重试保留离线/认证失败等现有状态，手动 r 刷新才重新显示 loading；持续 degraded 只在状态变化时通知。全局事件兼容 core 的 warn 级警告，WS 握手首帧不触发重复读取；当前 core 不发布 config 变更事件，配置变化依赖轮询，后续本地写入需显式刷新。

颜色与方形边框按原型，80×24 隐藏菜单文字、可选列和常驻详情，120×35 显示菜单/列表/详情。S/K/P/Y 是普通字符图标替代。右上角通知不参与焦点，普通结果 2 秒消失、常驻消息同 ID 更新；最多保留 16 条最终消息，运行消息直到完成都保留，不因显示拥挤丢失。模态打开时通知压为顶部一行，正文和确认键优先可见。四页的双语屏幕基准见 internal/tui/testdata；用户字段按字符宽度裁剪，控制及 Unicode Cf 格式字符仅转义显示，协议原值保留。

真实 core 协议联测：先构建 `core/cmd/core` 为 `/tmp/knot-core-tui`，再在 cli 运行 `KNOT_TEST_CORE_BIN=/tmp/knot-core-tui GOWORK=off go test ./internal/coreclient -run TestCoreBinary`。测试仅通过公开 API 在临时目录启动受控 core；普通测试未设置此变量时明确跳过此组合场景。

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

精确英文页面关键字优先：`knot key` 打开页面，`knot ssh key` 直连同名别名。单个其他位置参数调用 core resolve，不在本地判断别名是否存在；settings/status/files 不注册页面。多余参数、空参数、未知 flag、缺少 ssh alias 返回 2。没有 `--json`、旧 Cobra 命令树或补全。

`LaunchContext` 的 mode、initial_page、server_ref、return_policy 通过只读方法访问，controller 必须传递其退出策略。server_ref 此时仅是未解析别名；真实连接必须采用 core resolve 返回的持久服务器 ID。

help/version 不检测终端、不读取启动目录、不发现 core。其余入口要求 stdin/stdout 同时为 TTY。alias 入口实际发现并 resolve，成功后仍提示 SSH 开发中并退出 1，不创建管理菜单；不存在 alias 返回本地化错误 1。前端不会自动启动 core，也不会读写业务配置或偏好文件。

退出契约：远端 exit-status 0–255 原样保留，缺失/越界状态返回 1；本地连接/协议失败 1，参数错误 2，主动取消 130。管理 TUI 保留单次 SSH 结果继续运行。当前提供解析与状态转换边界，实际远端状态和交接验证属于 SSH 阶段。

## 语言与默认值

启动只检测一次语言：首选语言主标签为 zh 则中文，其他或查询失败则英文，不向后寻找支持的中文。命令、别名、路径、指纹、PTY bytes 不翻译。中英文 catalog 编译内置，未知 key 与缺少模板参数有显式诊断和通用本地化回退。

- Linux/Unix：LC_ALL → LC_MESSAGES → LANG；非空且非 C/POSIX（包括 C.UTF-8）时，LANGUAGE 的第一个非空项可覆盖。空 locale 忽略 LANGUAGE。
- Windows：`GetUserPreferredUILanguages(MUI_LANGUAGE_NAME)` 查询当前用户；失败回退英文，不按区域或代码页推断。
- macOS：`/usr/bin/defaults read -g AppleLanguages`，固定参数不经过 shell，1 秒期限、16 KiB 输出上限，无 CGO；失败按上述 locale 环境规则/英文回退。原生 macOS/Windows 查询尚待执行，交叉构建只证明编译。

固定默认值为 forward_agent=true、连接时清屏和前端启动目录。core 自身 idle/keepalive/log/recent 等默认值不由前端重写。

阶段状态、验证和续接说明见工作区根目录的 [开发进度](../../docs/cli-tui-progress.md)。
