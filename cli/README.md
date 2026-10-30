# Knot CLI / TUI

独立 module `knot-cli`，工具链为 Go 1.27.1。前端通过 core 的公开 HTTP/WS API 工作，已实现中英文根 TUI、真实资源列表、搜索、详情、离线/认证错误及事件恢复。资源管理、转发管理和真实 SSH 原生终端已接入；SFTP 文件页和文件传输仍在后续阶段。

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

离线、认证失败和协议错误有独立状态，由后台轮询自动重试；不自动启动服务。事件断流以 GET 和重新订阅恢复，根模型每 4 秒复核实际状态；请求最长 15 秒，旧页面/请求结果不会覆盖新状态。完整响应重新创建 DTO，省略字段清空旧关联。

## 原型交互

当前采用“晨露”暗色精调色板：背景 `#101313`、正文 `#d8dcd9`、次要文字 `#97a29b`、普通边框 `#525d56`、强调绿 `#abd6b7`、标签搜索蓝 `#97c2e2`、错误红 `#dda79e`、选中底色 `#2d4237`。此次仅调整 7 个色值，焦点、选中、弹框与通知规则及输入控件默认样式保持原样。

默认入口聚焦左菜单；页面直达聚焦右操作区。左菜单 ↑↓ 循环切页，→/Enter/Tab 进入操作区；在菜单按 / 搜索或 n 新增，会直接定位右侧并执行操作。同步密码未配置时，菜单仅支持 s 直接打开密码表单；已配置后支持 / 和 n。其他业务字母键在菜单无效。操作区 ←/Esc/Tab 返回菜单，↑↓/Home/End 选行，/ 搜索。右侧快捷键说明只随页面及同步密码门槛变化，不随焦点或列表加载状态变化；移除 ? 帮助、r 刷新、i 详情和服务器 t 转发入口。密钥/代理/同步列表 Enter 打开详情，服务器 c/Enter 连接 SSH。

服务器默认按 core 的 last_used 排序，仅别名子串搜索；输入框内 / 在开头切换标签子串搜索，边框变为蓝色；删除开头的 / 后恢复别名搜索及普通边框颜色。Esc/Enter 保留查询并回列表，Tag 搜索的蓝色边框也保留。刷新按资源 ID 保留选择。同步密码未设置时显示原型门槛，仅保留 s 的业务入口及通用导航。

左菜单 Esc 或菜单、操作区、搜索及普通模态中的 Ctrl+c 弹出“是否退出”确认，默认选择“否”。方向键切换“是/否”，Enter 确认，Esc/N 返回原页面，Y 或再次 Ctrl+c 确认退出；取消退出保留草稿及焦点，后台请求结果继续正常处理。确认退出及外部 SIGINT/SIGTERM 按取消退出 130。统一表单组件支持 ↑/↓ 切换字段，末项 ↓ 不提交；Tab/Enter 下一字段及末项提交功能保留，快捷键说明只显示方向键及 Ctrl+s 保存。支持 Shift+Enter 换行、多行粘贴、必填定位、忙状态与 Y/N/Esc 草稿路由。新增/编辑标题按资源类型翻译；页签以背景色标记选中项，字段间留白并为输入框加边框。表单弹框横向居中，顶部固定在屏幕第 4 行，高度随当前内容变化；切换页签时标题与页签不再上下移动，内容过长时继续在表单内滚动。切换字段后仍显示窗口内可容纳的已填字段，只在空间不足时滚动，返回前面字段可继续编辑原值。主机信任/传输/关闭 SFTP 专用模态只接受 Enter/Esc，选择器每页 5 项且翻页不穿透；资源表单和删除确认已使用这些组件；连接挑战及文件业务在后续阶段接入。所有业务确认均检查模态所属 core 实例，重启后保持旧框供取消、禁止用旧 ID 提交到新实例。

后台重试保留离线/认证失败等现有状态；持续 degraded 只在状态变化时通知。全局事件兼容 core 的 warn 级警告；session.error 由连接/终端结果及 Agent 重试流程处理，不重复转为通用 warning。返回原生 SSH 之前保留的临时通知会在重建 TUI 时重新启动关闭定时器。WS 握手首帧不触发重复读取；当前 core 不发布 config 变更事件，配置变化依赖轮询，本地写入成功会立即刷新。

颜色与方形边框按原型，80×24 隐藏菜单文字、可选列和常驻详情，120×35 显示菜单/列表/详情。详情使用灰色字段标签、普通正文值，字段组之间留空行，值缩进显示；右侧功能区和详情区的标题嵌入上边框并靠左显示，标题下留一行空白，快捷键及正文从下一行开始；服务器详情仅保留认证方式、代理和跳板链，不重复显示别名、连接目标、标签、最近使用或密码/密钥状态；密钥和代理详情同样不重复显示别名及列表已有字段：密钥详情保留引用服务器、私钥材料、指纹和口令保护状态，代理详情保留引用服务器和密码状态；同步详情保留绿色加粗别名。服务器列表未选中行不再显示圆点。菜单使用 🖥️/🔑/🔀/🔄/⚙️ 图标。同步页面标题为 Sync（中文为“同步”）。搜索框带边框，获得焦点时边框高亮，下面保留带列标题的边框表格；空列表和无搜索结果时同样展示表格，不再显示通用资源列表标题和首次配置提示。右上角通知不参与焦点，普通结果 2 秒消失、常驻消息同 ID 更新；最多保留 16 条最终消息，运行消息直到完成都保留，不因显示拥挤丢失。模态打开时通知压为顶部一行，正文和确认键优先可见。五页的双语屏幕基准见 internal/tui/testdata；用户字段按字符宽度裁剪，控制及 Unicode Cf 格式字符仅转义显示，协议原值保留。

真实 core 协议联测：先构建 `core/cmd/core` 为 `/tmp/knot-core-tui`，再在 cli 运行 `KNOT_TEST_CORE_BIN=/tmp/knot-core-tui GOWORK=off go test ./internal/coreclient -run TestCoreBinary`。测试仅通过公开 API 在临时目录启动受控 core；普通测试未设置此变量时明确跳过此组合场景。

菜单在 Sync 下方提供设置页，右侧为“客户端设置”和“核心设置”两个页签。功能区内用 [ / ] 切换页签，也接受中文输入法的【 / 】及全角［ / ］。客户端设置目前仅有语言，使用与服务器认证方式相同的 `‹ 当前选项 ›` 控件。进入功能区后，↓ 或 Enter 聚焦语言控件，←/→ 轮换 English / 中文；↑ 离开控件焦点，随后 ← 返回菜单、→ 从菜单进入功能区。空格和 Enter 不切换语言。快捷键栏只显示页签切换和 Ctrl+s 保存，不显示“← 菜单”及“空格/Enter 切换语言”。Ctrl+s 写入 TOML 成功后应用语言并自动将焦点返回左侧菜单。修改后按 Esc、Tab 或在控件未聚焦时按 ← 离开，复用表单的 Y 保存 / N 放弃 / Esc 返回编辑提示；保存失败保留草稿和原来的界面语言。核心设置暂为空白。设置页无需 core 在线；语言保存在 `$XDG_CONFIG_HOME/knot/client.toml`，未设置 XDG_CONFIG_HOME 时，三平台均使用 `~/.config/knot/client.toml`，格式为 `language = "en"` 或 `language = "zh"`。该文件与 core 的 config.toml 独立，启动时只读，不存在时不创建。所有页面与模态的快捷键统一使用 `key 说明`，按键为绿色、说明为灰色，页签提示合并为 `[] 切换页签`。

## 资源管理

操作区 n 新增，服务器/代理/同步页 e 编辑，d/Delete 删除并确认。密钥不增加编辑快捷键；x 清材料并保留记录和引用，删除被引用密钥的冲突由 core 返回。同步页 s 设置/确认/清除同步密码，a 设置或取消默认服务，p 管理凭据；未设置同步密码时只开放 s 及通用导航。

表单中 ←/→/Space 切换枚举和资源选择，显示别名、提交稳定 ID；输入框内 ←/→ 编辑文字，↑/↓ 切换字段。服务器保留基本/代理/跳板/转发四页签，[ 上一页、] 下一页，Ctrl+s 保存。新增服务器默认密码认证，新增、编辑和密码表单不再提供“清除已存密码”，密码留空保留已有值。在跳板/转发页签 Ctrl+n 新增、Ctrl+d 移除当前行，跳板用 Ctrl+↑/↓ 调序；跳板排除自身并检查循环。known_hosts 等未展示普通字段读取后保留。

密钥新增可选文件或粘贴；文件只提交绝对路径供 core 读取。PASSPHRASE_REQUIRED 保留草稿并定位临时口令，成功后关闭敏感表单，列表只显示实际元数据。代理草稿用 Ctrl+t 测试（列表仍用 t），可选目标 host:port；不先保存，结果区分 TCP 端点、SOCKS5 协商和目标隧道，关闭表单取消本次测试。代理及 WebDAV 的已有密码留空保留，清除使用明确的选择项。

同步只管理 WebDAV/S3 配置和凭据，不执行同步。S3 新对象键为 config.toml.enc；编辑 Endpoint 时 region 为空才填 auto，已有值保留。Access Key ID/Secret Access Key 成对输入或清除；换类型的旧字段/凭据由 core 原子清理。

服务器页已移除 t 转发快捷入口；转发管理组件保留，组件内 [ / ] 切换保存定义/运行实例，r 刷新；运行列表也通过全局事件及轮询复核。保存定义页 e/n 直达服务器转发页签，d 单独确认删除定义；修改只对以后新 transport 生效。运行页 n 新增临时规则，默认让 core 选择最新已建立目标连接，也可明确选择连接；s 启动、x 停止、d 删除，均使用响应中的实际运行 ID/connection ID。显示实际监听、类型、目标、来源、状态、错误和连接时间；202/保存定义不表示运行 active。停止/删除运行实例与删除保存定义分别确认，无连接不会自动建立 SSH。

按本轮用户要求，写入保持顺序调用：普通资料 → 转发定义（服务器）→ secret。失败显示错误并保留草稿；再次保存会重新发送这些步骤，已确认创建的资源沿用 ID。没有跨请求事务、分阶段重试、回滚或结果未知后的自动复核。

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

help/version 不检测终端、不读取启动目录、不发现 core。其余入口要求 stdin/stdout 同时为 TTY。`knot <alias>` 和 `knot ssh <alias>` 使用独立控制台流程，实际发现并 resolve 为持久服务器 ID；不创建 Bubble Tea 程序，也不切换 TUI 屏幕。控制台输出连接提示，仅在需要信任主机指纹时询问 yes/no，确认后直接进入原生 SSH。发现、解析、连接或认证失败时立即输出本地化错误并退出 1，不等待 TUI 通知，也不进入认证重试表单。前端不会自动启动 core，也不会读写 core 业务配置；只读写独立的客户端偏好文件。

退出契约：远端 exit-status 0–255 原样保留，缺失/越界状态返回 1；本地连接/协议失败 1，参数错误 2，主动取消 130。管理 TUI 保留单次 SSH 结果继续运行。

## SSH 连接和终端

管理页 c/Enter 与 alias 直连共用 connection.Attempt 协议客户端，分别使用 TUI 和控制台处理交互。201 只代表受理，使用 GET 复核真实 connected 后才进入终端；回答主机信任挑战成功后继续等待。管理页等待期间 Esc 取消并 DELETE 已受理的会话，连接失败留在服务器页。alias 直连关闭 allow_auth_retry，连接或认证失败直接输出错误并退出；Ctrl+c 取消等待及控制台指纹确认，返回 130，并清理已受理的会话。

管理入口的主机信任框显示服务器、主机、密钥类型和指纹，只接受 Enter 信任并保存或 Esc 取消。后台失败/超时保留原框；迟到确认按实际机器码报告超时、连接失败或实例变更，不重新连接来接受旧指纹。认证表单支持密码、已导入私钥/临时口令和 Agent；默认记住成功凭据，不再提供选择开关，实际保存由 core 完成，不持久化私钥口令。↑/↓ 切换字段、Ctrl+s 提交，Tab/Enter 功能保留；Esc 直接取消连接，不进入保存草稿流程。应答后旧轮询不能重新打开上一轮挑战。

alias 入口采用类似原生 SSH 的指纹确认文本，显示主机、服务器别名、密钥类型和指纹；主机身份变化时另行输出明确警告。必须输入完整 yes 才信任并保存，no、空行或 EOF 拒绝；确认绑定显示的 challenge_id，过期或 core 实例变化时失败退出。控制台沿用终端原生回显和行编辑，确认读取不预取后续 SSH 输入，取消后先等待输入读取结束再交出终端。

Agent 转发按可用性尽力启用：core 的 agent_forwarding capability 不可用时直接跳过，不弹窗、不报通用 warning。若能力快照已过期或远端拒绝转发，自动关闭转发并重试一次；新的尝试沿用已解析服务器 ID，仍遵循主机信任和认证流程，真实连接失败照常显示。Agent 状态留待后续“状态”菜单展示。

Bubble Tea 完全结束并释放 stdin 后才启动 raw bridge；退出桥接时先取消并等待输入/resize worker，恢复终端后才重新启动 TUI。binary 原样收发，Ctrl+c/Ctrl+d 送远端，支持 vim/top、ANSI/OSC7 与实际窗口尺寸更新。每次接入按固定默认清屏。终端标签名由 CLI 管理：启动显示 Knot，连接成功后使用 core 返回的服务器别名，SSH 退出、连接中断或 CLI 结束时恢复 Knot；管理入口和别名直连使用同一规则。通用方法 internal/terminal.SetTitle 可供后续功能复用，过滤标签文本中的控制字符后设置终端标题和图标名。前端不注入 shell hook 或改写远端输出。Linux 使用可取消的终端 reader；Windows 使用可取消 overlapped Console 句柄并保存/恢复输出模式；macOS/Windows 当前仅交叉编译，尚未做原生终端验收。

等待完整输出后的 session.exit 再处理退出码；session.error 不会提前截断尾部。缺少 exit frame 时通过 GET 查询结果，有实际退出码则保留并提示可能漏输出；无真实退出状态返回失败。WS 中断保留活跃会话为 detached，显示实际会话 ID，当前没有自动重连或终端重新附着入口，可通过 core API 管理。明确的进程取消会断开本次会话。仅收集当前 SSH 会话的 session.warning 到有界队列，创建响应返回前的警告先由事件流缓存，取得会话 ID 后再匹配；其他会话的警告和 session.error 不进入退出警告队列。回 TUI 或退出前显示，不插进远端字节流。

手动验证：先启动 core、保存服务器，执行 `knot servers` 后按 c；远端运行 `stty size`、`top`、`vim` 并缩放窗口，执行 `exit 7` 应返回服务器页。再用 `knot <alias>` 登录并 `exit 7`，本地 `echo $?` 应为 7；Ctrl+d 应正常退出 shell，重复连接后终端输入与回显应正常。SFTP 双栏及上传下载尚未实现。

## 语言与默认值

启动优先使用 client.toml 中已保存的语言；未保存语言时只检测一次系统语言：首选语言主标签为 zh 则中文，其他或查询失败则英文，不向后寻找支持的中文。客户端设置文件格式错误或不可读时输出提示并回退系统语言。命令、别名、路径、指纹、PTY bytes 不翻译。中英文 catalog 编译内置，未知 key 与缺少模板参数有显式诊断和通用本地化回退。

- Linux/Unix：LC_ALL → LC_MESSAGES → LANG；非空且非 C/POSIX（包括 C.UTF-8）时，LANGUAGE 的第一个非空项可覆盖。空 locale 忽略 LANGUAGE。
- Windows：`GetUserPreferredUILanguages(MUI_LANGUAGE_NAME)` 查询当前用户；失败回退英文，不按区域或代码页推断。
- macOS：`/usr/bin/defaults read -g AppleLanguages`，固定参数不经过 shell，1 秒期限、16 KiB 输出上限，无 CGO；失败按上述 locale 环境规则/英文回退。原生 macOS/Windows 查询尚待执行，交叉构建只证明编译。

默认尽力启用 Agent 转发，并固定连接时清屏和前端启动目录。core 自身 idle/keepalive/log/recent 等默认值不由前端重写。

阶段状态、验证和续接说明见工作区根目录的 [开发进度](../../docs/cli-tui-progress.md)。
