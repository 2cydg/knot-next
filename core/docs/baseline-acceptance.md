# 基线验收证据

按 `docs/core基线开发计划.md` §19.2 记录每个工作项的验收证据。受控测试结果与真实环境结果分开列出，不互相替代。

## B03：PTY 数据、退出与 attach 生命周期

| 字段 | 内容 |
| --- | --- |
| 基线与变更 | 工作区变更（未提交）；实现文件 `pkg/session/backend.go`、`pkg/session/session.go`、`internal/api/http/session_handlers.go`、`internal/api/http/ws_writer.go`、`internal/testutil/sshserver/server.go`；删除 `pkg/session/attach.go`（未接线的重复实现）并回退 `github.com/gorilla/websocket` 依赖 |
| 场景 | P01–P08，测试文件见下表；层级：module 内单元 + 服务级（本机受控 SSH，真实 SSH 协议）+ HTTP/WS 契约 |
| 环境 | linux/amd64（WSL2），Go 1.27.1，受控 SSH server（`internal/testutil/sshserver`，loopback 随机端口），无需 Docker/互联网 |
| 验证 | 命令与结果见下；`go vet ./...` 无输出；`go test -race -count=20` 关键用例通过 |
| 结果 | 受控测试全部通过；真实环境 smoke 见 §真实环境 |
| 限制 | 跨流（stdout/stderr）不承诺全局顺序；未附着输出只保留有界缓冲（64 KiB）；慢客户端以有界关闭结束，不保证送达 `session.exit`；WS 库选型与标准客户端 RFC 契约测试属于 B12 |

### 场景到测试的映射

| 编号 | 场景 | 测试 |
| --- | --- | --- |
| P01 | ANSI、OSC7、CR/LF、NUL、中文及无效 UTF-8，跨 frame 分片 | `TestAttachForwardsPTYBytesExactly`（真实 SSH + WS，逐字节比对）、`TestSSHSessionPreservesPTYBytesExactly`、`TestPumpForwardsBytesWithoutInterpretation` |
| P02 | 输出大于缓冲后立即退出，exit 0/7 | `TestAttachDeliversTrailingOutputBeforeExit`（1 MiB，断言唯一 exit 且与 GET 一致）、`TestSSHSessionExitCodeIsRecordedAndReportedConsistently` |
| P03 | 多观察者同一结果、无 nil/0 抢读 | `TestExitResultBroadcastToOneOwner`、`TestExitOutcomeErrSemantics`、`-race -count=20` |
| P04 | 反复 attach/detach/re-attach | `TestAttachDetachReattachKeepsSingleOwner`、`TestSSHSessionReattachHasOneOutputOwner`、`TestSSHSessionRejectsSecondAttachment` |
| P05 | 第二客户端 attach | `TestAttachRejectsSecondClientWhileFirstStaysUsable`（升级前 409，第一客户端仍可用）、`TestSessionAttachRejectsClosedSession`（拒绝后无残留附着） |
| P06 | resize 多次、非法尺寸、close_stdin、signal | `TestAttachControlMessages`、`TestAttachInvalidResizeOverHTTPIsRejected`、`TestSSHSessionResizeSendsRealWindowChange`、`TestSSHSessionRejectsInvalidResizeWithoutTouchingRemote`、`TestSSHSessionCloseStdinIsIdempotent`、`TestSSHSessionSignalIsForwardedAndValidated` |
| P07 | 慢读、客户端断开、远端断网、服务侧清理 | `TestAttachSlowClientEndsBoundedAndKeepsSessionAlive`、`TestAttachClientDisconnectKeepsSessionUsable`、`TestAttachRemoteNetworkDropReportsFailure`、`TestAttachEndsWhenPoolDisconnects`、`TestSSHSessionClientDisconnectIsDistinctFromRemoteExit` |
| P08 | PTY echo/env/速度参数 | `TestSSHSessionRecordsPTYAndEnvironmentRequests`（服务端记录 pty-req 与 env）、`pty_test.go`（迁移旧 table-driven 用例） |
| 附加 | 未附着输出的有界缓冲与截断通知 | `TestAttachReportsTruncatedPreAttachBacklog`、`TestPumpReportsTruncatedBacklog`、`TestPumpDeliversBacklogToFirstAttachment` |

### 执行的验证命令

```bash
cd knot-next/core
GOWORK=off go build ./... && GOWORK=off go vet ./...      # 均无输出
GOWORK=off go test -count=1 -timeout=360s ./...            # 全部包 ok
GOWORK=off go test -race -count=1 -timeout=280s ./internal/api/http/   # ok
GOWORK=off go test -race -count=20 -timeout=1480s -run 'Attach|Exit|Pump|Overflow|PTY|Close' ./pkg/session ./internal/api/http   # ok
GOWORK=off go test -count=40 -run TestAttachReportsTruncated ./internal/api/http   # ok（无偶发挂起）
```

### 缺陷修复记录（本批次实际修复）

1. `computeExitResult` 无法识别包装错误与 `*ssh.ExitMissingError`，`exit_7` 用例失败；改为接口断言并补齐 exit-signal（`128+signum`）、缺 exit-status、客户端关闭、网络错误四类语义。
2. `watchInteractive` 先广播结果再提交 session 终态，观察者可能读到旧 `exit_code`；改为先提交后广播，并在客户端已关闭的路径上也提交结果，避免观察者永久阻塞。
3. 输出泵只读 stdout：调用 `StderrPipe()` 后 x/crypto/ssh 不再排空 stderr，远端写 stderr 会耗尽窗口；改为单泵多流，并保持流内顺序。
4. attach 冲突先升级再报错、`session.exit` 不等输出 relay 排空、relay 失败无法解阻 `conn.ReadFrame()` 导致附着权不释放；改为升级前校验与占用、FIFO 单写者、退出前有界排空、任一路径失败即关闭连接。
5. 慢客户端无写期限：`WriteFrame` 持锁永久阻塞；改为有界出站队列 + 每次写入期限 + 溢出即明确结束。
6. `closeSessionLocked` 无幂等保护且在服务锁内关闭 SSH 会话；改为终态幂等、结果一次性提交、关闭动作移出锁。
7. 未附着期间的输出被直接丢弃（连 shell 提示符都会丢）；改为有界缓冲 + 截断通知，已投递字节不重放。

## 真实环境

见下方各批次的真实环境小节。真实端点仅供 Agent 手动联调，其地址与凭据不写入单元测试、fixture、CI 配置或默认配置。

### B03 真实 PTY smoke（2026-10-07）

| 字段 | 内容 |
| --- | --- |
| 场景 | 真实 SSH 密码连接、PTY attach、输入输出、ANSI/CRLF/UTF-8 字节保真、resize、退出码 0/7 与 GET 一致性 |
| 环境 | `192.168.0.22:5022`（§19.1 用户提供端点）；隔离的临时 `XDG_CONFIG_HOME`/`XDG_STATE_HOME`，host key 策略 `accept-new`；core 由 `cmd/core` 现场构建，监听 127.0.0.1:17911；密码仅通过环境变量传入 |
| 验证 | `go build -o knot-core ./cmd/core` + 临时驱动程序（仅使用公开 HTTP/WS 接口，stdlib 实现，位于仓库外 `/tmp/knot-b03-smoke/`） |
| 结果 | 9 项检查全部通过：连接成功 state=connected；`echo` 往返收到 marker；`printf '\033[1;31mCOLOR\033[0m 中文终端\r\n'` 的 ANSI 转义、CRLF、UTF-8 中文均原样到达；resize 40x120 收到 `session.resized`；`exit 7` 后收到唯一 `session.exit` 且 `exit_code=7`；`session.exit` 之后连接关闭且无后续字节；`GET /v1/sessions/{id}` 返回 `state=closed`、`exit_code=7`，与消息一致 |
| 限制 | 单端点、单会话冒烟；未覆盖真实环境下的慢客户端与断网场景（这些由受控测试覆盖）；远端实际 shell 的 OSC7 行为未单独断言 |

记录中不含密码、token 或私钥。驱动脚本与临时配置均位于仓库外，不会提交。`<endpoint>` 结果不替代受控测试：受控测试覆盖 P01–P08 全部矩阵，真实环境只补充真实 PTY 的证据。


## B04：exec context、期限与输出收尾（2026-10-07）

本批次实现已落地，**B04 尚未完整验收**。2026-10-07 审阅确认并修复 fixture 挂起、待收尾结果被裁剪、完成/取消竞争；测试及文档建议同时落实。TCP 可运行的审阅环境已提供 E01/E02/E07/E08 证据，当前修复环境仍拒绝监听，两次环境与代码状态分别记录。极端协议等待、代理/跳板 exec 组合与外部 smoke 仍有缺口；不据此宣布基线完成。

| 字段 | 内容 |
| --- | --- |
| 基线与变更 | `55e19c2` 之后工作区 diff，未提交；新增 `pkg/session/exec.go`，接入 HTTP request context、lifecycle service context 与 core shutdown；扩展受控 SSH exec fixture 和内存 transport |
| 来源 | 参考旧 `knot/pkg/daemon/handler_exec.go`（`e0b4d51eea6647e192371059381039b99fb301a2`）的双流限长规则，未复制其取消实现；内存 transport 参考当前 `pkg/sftp/review_transport_test.go` buffered pipe，新模块内适配，无新依赖 |
| 环境 | Linux amd64，Go 1.27.1；静态测试 crypto，人工测试凭据；真实 SSH/SFTP 协议在内存 transport 执行，另保留随机 loopback 的服务/HTTP 测试 |
| 实现 | `ExecContext`；全链路 timeout/非负与溢出验证；15 秒 channel open 阶段期限；KILL 尝试 + channel close + 有界等待 Run/close/signal；实际 worker/ref 跟踪；独立 512 KiB 双流同步快照；framework_code/cleanup_error |
| 通过 | 内存真实 SSH 双流/退出/取消/共享 shell+SFTP；50 次输出中取消；定向 race ×20；无监听 HTTP context/参数测试 race ×20；全包测试编译；vet；Linux build；Windows amd64 session/HTTP 测试编译 |
| 未通过/未执行 | 初次执行和本次修复环境的 TCP/TCP6 监听被拒绝；审阅环境允许监听，其全量失败另有 fixture barrier 挂起（已修复）。TCP 跨层审阅证据见下节；本修复状态下未重跑成功。真实 OpenSSH >30 秒、真实远端停止与 Windows 原生未运行 |
| 协议边界 | x/crypto/ssh 无法撤回未确认 channel open，也无法本地完成永久不回应 channel close 的 Wait；返回 cleanup_error，继续持有 ref 并跟踪 worker，transport 退出后才能最终收尾。已验证预算和迟到清理，尚未达到 E06 的“任何阶段无遗留”完整要求 |

### 场景与正式测试

| 编号 | 测试与结果 |
| --- | --- |
| E01 | `TestExecStreamsAndExitStatus`（真实内存 SSH，0/7、双流逐字节）通过；`TestServerExecReturnsRemoteExitSeven`（TCP SSH + HTTP）审阅环境通过，当前修复环境未重跑成功 |
| E02/E03 | `TestExecCancelWaitsForRunAndKeepsSharedChannels`（真实内存 SSH/SFTP、忽略 signal、取消与期限、worker settled、另一个 shell/SFTP 可用）通过；`TestExecServiceCancellationReleasesReference`、`TestServerExecHTTPDisconnectClosesRemoteChannel` 审阅环境通过，当前修复环境受监听限制 |
| E04 | `TestExecCancelWhileWritingOutput` 首次写成功后取消，50 次，远端 worker done + 本地 settled + 输出上限，race ×20 通过 |
| E05 | `TestExecTruncatesEachStreamAtBoundary`：每流 512 KiB、+1、2 MiB；前缀完整、独立截断、Writer 返回原长度，race ×20 通过 |
| E06 | `TestExecCanceledChannelOpenClosesLateResult`、`TestExecChannelOpenHasStageDeadline`、`TestExecCancelDuringStartWaitsForWorker` 通过；`TestExecConnectionDeadlineDoesNotStartCommand` 的原 fixture 在可监听环境 cleanup 挂起（已修复）；审阅通过可释放 barrier 复跑确认 timeout/未启动 command；修复版 TCP 用例当前受监听限制；代理/跳板完整组合未补齐 |
| E07 | `TestExecContextValidationAndShutdown`、`TestExecCleanupTimeoutTracksActualWorker` 通过；`TestExecServiceShutdownStopsUnlimitedCommand`（完整 service/lifecycle ctx）审阅环境通过，当前修复环境受监听限制，完整进程退出仍待验收 |
| E08 | `TestExecMissingExitStatus`、参数/溢出验证通过；`TestExecTrustAndAuthenticationFailures` 审阅环境通过，当前修复环境受监听限制 |
| HTTP 回归 | `TestServerExecUsesRequestContext`、`TestServerExecRejectsInvalidTimeout`、原 `TestServerSessionExecValidation` race ×20 通过；overlay 恢复旧 `Exec(body)` 后 request context 测试确定失败 |

### 实际命令与结果

执行目录 `knot-next/core`，均设置 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`。

- `go test -race -count=20 -timeout=90s -run '^TestExec(Streams|Truncates|Cancel|Missing|Context|Cleanup)' ./pkg/session`：通过；日志 `/tmp/knot-b04-exec-race20.log`。
- `go test -race -count=20 -timeout=60s -run '^TestExecChannelOpenHasStageDeadline$' ./pkg/session`：通过。
- `go test -race -count=20 -timeout=90s -run 'TestServerExecUses|TestServerExecRejects|TestServerSessionExecValidation' ./internal/api/http`：通过。
- `go test -overlay=/tmp/knot-b04-handler-overlay.json -count=1 -run '^TestServerExecUsesRequestContext$' ./internal/api/http`：预期失败；仅 overlay 改回旧调用，不修改工作区；日志 `/tmp/knot-b04-handler-regression.log`。
- `go test -count=1 -timeout=60s ./...`：环境失败，不作为通过证据；日志 `/tmp/knot-b04-full-test.log`。
- `go test -run '^$' ./...`：全部编译通过，仅编译，不构成运行验收。
- `go vet ./...`、`go build -o /tmp/knot-b04-core ./cmd/core`：通过。
- `GOOS=windows GOARCH=amd64 go test -c` 分别编译 `./pkg/session` 和 `./internal/api/http`：通过，不构成 Windows 原生验证。

不改为全量 skip、不写入真实服务器地址/密码、不提交临时 smoke 配置。此条是 B04 当时记录；B05 后续进度见下方子批次，B06 及历史 B01–B03 缺口继续保留。


### B04 审阅逐项修复与证据（2026-10-07）

审阅来源：workspace `docs/running/B04-implementation-plan.md` 第 56 行起。原始日志 `/tmp/knot-b04-full-test.log` 明确含 `socket: operation not permitted`；审阅日志 `/tmp/b04-fullsuite.log` 明确含 `TestExecConnectionDeadlineDoesNotStartCommand` 的 barrier 清理挂起。因此“所有失败只是权限问题”的归纳不完整；“历史权限错误不存在，全部替换为单测挂起”也与原日志不符。保留两类事实，当前修复环境复跑依然拒绝监听，不将其他环境的通过冒充本次修复后的运行结果。

| 审阅项 | 判定与处理 | 回归证据 |
| --- | --- | --- |
| 2.1 fixture barrier 挂起 | 存在，已修复。`waitBarrier` 同时观察 Close，返回 false 后 handshake/auth/subsystem 停止；Close 跟踪并关闭未握手 transport，避免解除 barrier 后仍阻塞于握手。E06 用例另加 LIFO barrier cleanup | `TestServerCloseReleasesEveryStageBarrier`（3 阶段）、`TestServerCloseReleasesPendingHandshakeTransport`，race ×20；恢复旧 waitBarrier 时前者确定失败 |
| 2.2 验收归因 | 部分采纳：追加允许监听的审阅证据和真实 fixture 缺陷，保留初次及本轮权限失败事实 | 初次 `/tmp/knot-b04-full-test.log`；审阅 `/tmp/b04-fullsuite.log`、`/tmp/b04-skip.log`；本轮 `/tmp/knot-b04-review-full.log` |
| 3.1 pending 记录裁剪 | 存在，已修复。裁剪跳过活跃 exec；shutdown 快照持有 `execOperation` 的结果，worker settled 后即使记录被裁剪也不丢诊断 | `TestExecPrunePreservesPendingCleanup`、`TestExecShutdownKeepsResultAfterHistoryPrune`，race ×20；恢复旧裁剪条件时前者确定失败 |
| 3.2 完成/取消竞争 | 存在，已修复。取消分支复查 Run 结果，已发布结果优先；只对未完成的执行尝试 signal | `TestExecCompletedResultWinsConcurrentCancellation`：1000 次双就绪，race ×20；恢复随机选择时确定失败 |
| 3.3 goroutine 内 Fatal | 存在，已修复。SSH channel setup 移到测试 goroutine，后台只执行并回传结果；Start 收尾断言也移回测试 goroutine | 内存协议取消和收尾回归 |
| 3.3 包级期限 override | 串行测试下无 race，补显式禁止 t.Parallel 的注释，保留短预算以确定性验证真实分支 | 定向 race ×20 |
| 3.3 内存 deadline | 无需实现 socket deadline：此 fixture 验证协议和控制点；补充三种 SetDeadline 都无效的注释，不将其算作 TCP 期限证据 | `NewMemory` 注释，TCP 与内存场景分开 |
| 3.3 未使用 exec/exit gate | 两个 setter 已存在于基线，但确实没有用例；保留通用控制点并新增 gate 覆盖 | `TestExecFixtureGlobalGates`：放行 exec 前不 started、放行 exit 前不完成，exit 7 |
| 四 Exec 示例/id | 已补失败示例包含 framework_code/cleanup_error，成功示例按 omitempty；说明 id 仅诊断/关联，无 GET 查询 | `docs/api/sessions.md` |
| 四/五 正确契约与设计 | timeout 上限、阶段期限、输出限长、context、HTTP 状态、ref 所有权保留；没有证据要求改动。收尾跟踪补强诊断保存 | 原回归及本轮 race |
| 六 剩余外部/极端场景 | 是验收缺口，继续如实保留，不用本次修复宣称完整 B04 | 真实 >30 秒、代理/跳板 exec、Windows 原生、永久不响应 channel open/close 的回收未闭环 |

允许监听的审阅环境（修复前代码）已通过：`TestExecServiceCancellationReleasesReference`、`TestExecServiceShutdownStopsUnlimitedCommand`、`TestExecTrustAndAuthenticationFailures`、`TestServerExecHTTPDisconnectClosesRemoteChannel`、`TestServerExecReturnsRemoteExitSeven`；审阅的可释放 E06 barrier 场景也通过。跳过挂起用例的全量通过仅用于定位，不作为 B04 全量验收完成。

本轮修复验证（Linux amd64 / Go 1.27.1，`GOWORK=off GOPROXY=off GOCACHE=/tmp/knot-plan-20261006-go-build`）：

- 定向 session + fixture `go test -race -count=20`：通过，日志 `/tmp/knot-b04-review-race20.log`。
- 无监听 HTTP context/validation `go test -race -count=20`：通过，日志 `/tmp/knot-b04-review-http-race20.log`。
- 旧行为 overlay：barrier/裁剪/完成竞争的三个回归预期失败，日志 `/tmp/knot-b04-review-{barrier,prune,completion}-before.log`；不改工作区生产代码。
- `go test -count=1 -timeout=75s ./...`：环境失败（监听被拒绝，进程 readiness 无法完成），日志 `/tmp/knot-b04-review-full.log`，不 skip 后声称通过。
- `go test -run '^$' ./...`、`go vet ./...`、Linux core 构建与 Windows amd64 session/HTTP 测试编译：通过；编译不构成 Windows 原生或 TCP 运行验收。

## B05 子批次：真实 SFTP 传输与事件恢复（2026-10-07）

本次完成 F02、F03、F04 的非取消批量结果和 F07 的双会话隔离部分。**整个 B05 尚未完成**；F01、F05/F06、F07 慢消费者及并发快照、F08/F09 等组合验收留待下一批。

| 字段 | 内容 |
| --- | --- |
| 基线与变更 | `f7bb150` 之后未提交 diff；新增 `tests/integration/sftp_{fixture,client,transfer}_test.go`；扩展 `internal/testutil/sshserver/server.go`；修正 `docs/api/sftp.md` |
| 来源 | 参考当前基线 HTTP transfer snapshot、SFTP backend close 与 cancel 测试；复用现有受控 SSH fixture。未从旧仓库另复制实现，未新增依赖 |
| 层级 | 真实 loopback TCP SSH、密码认证、SFTP subsystem 和 SFTP 协议 request server、真实 HTTP/WS；没有启用 core 的 local-sandbox。受控远端使用 `github.com/pkg/sftp` 的内存文件处理器；不是外部 OpenSSH/操作系统文件权限验收 |
| 客户端 | 独立 JSON 模型，只通过公开 HTTP/WS 创建 profile、写 secret、创建会话、传输、GET/DELETE、订阅和恢复；内部服务对象仅用于 fixture 装配与清理 |
| 环境 | Linux amd64、Go 1.27.1；静态测试 crypto，临时 Layout 与人工密码；HTTP bearer 认证，WS 使用已有 `golang.org/x/net/websocket` 标准客户端，测试 Origin 显式允许 |
| 结果 | 初版 11 个真实协议叶用例 race ×20 通过；审阅修复后另含 2 个 WS 客户端期限用例，合计 13 个叶用例 race ×20 与 core 全量测试通过；vet、core build 与 diff 检查通过 |
| 限制 | 未进行外部 SSH smoke 或 Windows 原生运行；普通取消/永久阻塞 I/O 下显式 session 关闭的完整隔离与 worker 回收、路径/缓存及历史裁剪仍未在本批组合验收。此 fixture 未绑定进程生命周期 context、未接 API shutdown 回调、runtime holder 为 nil；只在 cleanup 直接调用 service Shutdown，不作为进程/API shutdown 中止 in-flight 连接、readiness 或 discovery 的证据 |

### 场景与断言

| 编号 | 测试 | 证据 |
| --- | --- | --- |
| F02/F03 | `TestSFTPProtocolFileRoundTrip`（empty/small/large） | 每种文件上传与下载，逐字节核对 NUL、无效 UTF-8、中文等内容；大文件超过 32 KiB 复制缓冲。GET 进度不越界，终态 total/copied/files_done 与时间正确；先订阅再 POST 的 completed 事件与 GET 一致；完成后订阅直接得到两个完整终态快照 |
| F03/F07 | `TestSFTPProtocolTransferRecoveryAndIsolation` | 远端 write barrier 到达后断开 WS；原 ID GET 仍 running，重新订阅得到相同快照。两个 session 的快照隔离，错误组合的 GET/DELETE 返回 404；另一个 session 在阻塞期间传输成功；status 确认单个 SSH entry、引用数为 2。放行后恢复 completed、远端仅打开文件一次、任务列表只有原 ID。审阅修复后连续两次 DELETE，每次再 GET 与完整 final 比对，并重订阅核对快照，验证保留状态和完成时间不变；下载内容准确 |
| F04 | `TestSFTPProtocolBatchResults`（upload/download × completed/partial_failed/failed） | 全成功为两项，部分失败先失败再成功验证继续执行，全失败保留两项错误；HTTP 202 后 GET 返回正确终态，逐项 Source/State/Error 和成功文件计数正确；已规划文件的 aggregate total/copied 一致，completed/partial_failed/failed 事件类型/数值与 GET 一致，晚订阅保留 items，成功项目内容可核对 |
| fixture 关闭 | `TestSFTPProtocolFixtureCloseReleasesWriteGate` | 不释放 write barrier，直接关闭 SSH fixture；protocol worker 在 5 秒内结束，传输收敛 failed/canceled、copied=0、files_done=0，没有误报 completed |
| WS 客户端期限 | `TestSFTPProtocolTerminalEventDeadline`（idle/continuous_progress） | 独立 WS helper 回归，不包含 SSH/SFTP。无消息或持续 progress 时，等待仍使用同一个 60 ms deadline，返回可识别的 timeout 原因与 transfer ID；避免每次读取重置成新的 5 秒 |

事件与 GET 数值一致的组合证据仅覆盖已完成/已失败路径（包括 partial_failed），不证明 worker 私有进度尚未发布时的“最终进度先合并、再发布终态”顺序。生产 `finishTransferFromWorker` 当前顺序正确；下一批 F05/F06 需控制中途取消，让 worker 私有进度领先最后一次公开 progress，再同时断言 canceled 事件和 GET 的最终数值。本批 fixture 关闭用例未核对 canceled 事件，不能补足此缺口。

### 实际命令与结果

执行目录 `knot-next/core`，设置 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`。

- `go test -count=1 -timeout=60s -run '^TestSFTPProtocol' ./tests/integration`：初版 10 个场景通过（0.829 秒）；最终 11 个场景见下述 race 和全量。
- `go test -race -count=20 -timeout=90s -run '^TestSFTPProtocol' ./tests/integration`：通过（20.283 秒），包括新增 fixture 关闭回归及双项批量全成功。
- `go test ./...`：全部通过（其中 `pkg/session` 7.155 秒、`pkg/sftp` 8.401 秒、`tests/integration` 1.477 秒）。
- `go vet ./...`、`go build -o /tmp/knot-b05-core ./cmd/core`：通过，无错误输出。
- `git diff --check`：通过。

普通沙箱第一次执行新增测试被 `listen tcp 127.0.0.1:0: socket: operation not permitted` 阻止；随后授权在允许 loopback 监听的环境运行，实际协议与全量验收通过，没有 skip 用例。首次可监听运行还发现测试按旧文档错误断言 `backend=ssh`，实际为 `ssh-sftp`，已核查并修正测试和文档；没有以更改生产返回值掩盖差异。

本批没有发现需要修改 `pkg/sftp` 传输业务逻辑的问题；保留原 worker 私有进度、不可变后端视图、快照深拷贝和终态保护。下一批补真实 challenge/subsystem 失败与取消/关闭组合，再进入 B06 回收。

### 审阅修复后的验证

逐项结论见 workspace `docs/running/B05-transfer-recovery-plan.md` 的“审阅报告逐项核查与处理”。初版命令结果保留在上节；修复版使用同一环境与 Go 参数执行：

- `go test -count=1 -timeout=60s -run '^TestSFTPProtocol' ./tests/integration`：通过（0.959 秒）。
- `go test -race -count=20 -timeout=90s -run '^TestSFTPProtocol' ./tests/integration`：13 个叶用例全部通过（22.651 秒），无 race。
- `go test ./...`：全部通过（session 7.148 秒、sftp 8.396 秒、integration 1.630 秒）。
- `go vet ./...`、`go build -o /tmp/knot-b05-review-core ./cmd/core`、`git diff --check`：通过。
- 临时取消 overlay：在返回旧 snapshot 后改写共享终态和 CompletedAt，`TestSFTPProtocolTransferRecoveryAndIsolation` 预期失败，错误为 `late cancel rewrote retained terminal result`。日志 `/tmp/knot-b05-review-cancel-mutation.log`。
- 临时期限制 overlay：每次读取重新使用 `now + 5s`，`TestSFTPProtocolTerminalEventDeadline/idle` 预期失败，60 ms 预算被拖到 5.002 秒。日志 `/tmp/knot-b05-review-deadline-mutation.log`。

两个 overlay 位于 `/tmp/knot-b05-review-overlays/`，仅供 Go 测试临时替换源码，没有修改生产工作区文件。未新增依赖、未 Git 提交。`backend` 文档已改为创建时即为 `ssh-sftp`，createSession helper 同时检查 connecting 响应的 backend。

## B06：会话、历史、订阅与 worker 回收

| 字段 | 内容 |
| --- | --- |
| 基线与变更 | `9e7fd0fa0f9a878d9ce6ccf2bf94fe37207792ac` + 未提交工作区；新增 `internal/resourcepolicy/`、SSH/SFTP `retention.go` 及回归；修改 session/exec/SFTP/pool/core 和进程接线；全局订阅超限在 WS 升级前返回错误 |
| 来源 | 在当前 B01–B05 实现上修复；复用当前 `sshserver`、exec 延迟 channel、gated stdin、真实 SFTP handler 与 shutdown fixture，无旧项目代码复制，无新增依赖 |
| 环境 | Linux amd64，Go 1.27.1；module 保持 `go 1.26.2`；真实 loopback SSH/SFTP，随机端口、临时目录、人工凭据 |
| 结果 | R01–R07 本机验收通过，详细场景如下；无 Git 提交 |
| 平台 | Linux 实际运行；Linux/macOS/Windows × amd64/arm64 六目标构建通过。macOS/Windows 原生运行未执行，未将交叉构建记为运行验收 |

### 场景与实际断言

| 编号 | 测试 | 断言 |
| --- | --- | --- |
| R01 | 两包 `TestTerminalRetentionDoesNotBlockCreation`，`TestTransferHistoryCapacityAndActiveProtection`，`TestSessionMaintenancePrunesWithoutRequests` / `TestSFTPMaintenancePrunesWithoutRequests` | 注入时钟与小 policy，最旧终态优先删除，保留结果/时间可查，过期 ErrNotFound（HTTP 映射 404），活跃记录不删除；无新请求时定期清理也执行 |
| R02 | `TestSessionCreationBeyondLifetimeLimit` / `TestSFTPCreationBeyondLifetimeLimit` | 各创建关闭 1056 个 session，再创建成功；已收尾历史最多 1024，活跃数归零。大批次使用本地可控 backend，真实协议小批次由 R04/R06 补齐 |
| R03 | 两包 `TestTerminalRetentionDoesNotBlockCreation`、`TestTransferRetentionWaitsForWorker`、`TestTerminalHistoryWaitsForBackendRelease` | 活跃限额拒绝新建，关闭后可创建；Transfer 实际 worker 限额生效；释放中的终态仍持有 worker、不能裁剪，待释放 session 也有单独容量防止永久阻塞对象无限累积 |
| R04 | `TestSessionConcurrentCloseOwnership` / `TestSFTPConcurrentCloseOwnership`；`TestSessionCloseKeepsSharedReference` / `TestSFTPCloseKeepsSharedReference` | SSH/SFTP 各 50 轮同时关闭/重复关闭/远端退出或 pool 断开；每轮 worker 完成，ref 为零；同连接另一 owner 留下 ref=1 且仍可 resize/list，能发现重复 release 而不只检查 ref 未负 |
| R05 | `TestSessionSubscriberLimitsAndIdempotentCancel`、`TestSFTPSubscriberLimitsAndIdempotentCancel`、`TestGlobalSubscriberLimitAndClose`、`TestResourceSubscriberLimitsReturnConflict` | SSH/SFTP/transfer 16、CWD 8、global 64 的真实数量限额；慢消费者不阻塞，取消两次/关闭后取消安全，名额重用，channel 与 map 清理；HTTP 409 及 error code 验证 |
| R06 | `TestResourceShutdownAfterRepeatedWork`、`TestSFTPFollowCloseReleasesOwner`、`TestCanceledChannelOpenRetainsCleanupOwner`、`TestPoolShutdownWaitsForCreationAndCleanup`、`TestPoolShutdownWaitsForKeepaliveAndCallbackReentry` | 真实 SSH/SFTP 共 pool，多次 attach/follow/transfer/exec 后两次并发 shutdown；实际 exec/远端 worker 完成，连接和 ref 清零、订阅结束；迟到 channel/不遵守取消的拨号/阻塞 backend 不会被误报释放，预算不足明确报错，放行后再次等待成功；清理 loop 可结束 |
| R07 | `TestSessionCallbackReentryAndPanic` / `TestSFTPCallbackReentryAndPanic`，`TestSFTPFailureReleasesSubscribersAndFollower`，resourcepolicy callback 测试 | 回调能 GET 和 Shutdown；panic 后继续通知；follow cancel 在锁外可反入目标服务；回调队列有界，其他资源正常收尾 |
| 配置回收 | `TestPoolIdleReplacementKeepsNewOwner` + 既有 identity/route 回归 | 空闲旧身份被替换时关闭旧 client，避免按复用 key 发迟到 disconnect 误关新 owner；活跃身份仍用既有 revision 隔离，不在普通配置保存时 Clear |
| 失败路径 | `TestSessionFailureClosesSubscribersAndContext` / `TestSFTPFailureReleasesSubscribersAndFollower`、`TestSFTPTransportDropNeedsNoCallback` | connect/host key/auth 失败结束订阅、CWD 和 context；每个真实 SFTP client 自行观察 transport loss，资源回收不依赖通知送达 |

### 验证命令与工件

目录 `knot-next/core`；所有 Go 命令使用 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`。首次普通沙箱禁止 socket，随后在获准的可监听环境运行；未通过 skip 绕过协议用例。

```bash
go test -count=1 -timeout=180s \
  -coverpkg=./pkg/session,./pkg/sftp,./pkg/sshpool,./pkg/core,./internal/resourcepolicy \
  -coverprofile=/tmp/knot-b06-cover.out ./...

go test -race -count=1 -timeout=180s \
  ./pkg/session ./pkg/sftp ./pkg/sshpool ./pkg/core \
  ./internal/api/http ./tests/integration ./internal/resourcepolicy

B06_RUN='Test(Terminal|SessionSubscriber|SessionCallback|SessionConcurrent|SessionMaintenance|SessionFailure|SessionCloseKeeps|CanceledChannel|SFTPSubscriber|SFTPFollowClose|SFTPCallback|SFTPConcurrent|SFTPTransport|SFTPMaintenance|SFTPFailure|SFTPCloseKeeps|TransferRetention|TransferHistory|PoolShutdown|ResourceShutdown|GlobalSubscriber|ResourceSubscriber|HistoryPolicy|WorkerOwnership|CallbackQueue|CancellationCallback)'
go test -race -count=20 -timeout=240s -run "$B06_RUN" \
  ./pkg/session ./pkg/sftp ./pkg/sshpool ./pkg/core ./internal/api/http ./internal/resourcepolicy

go test -race -count=20 -timeout=30s -run '^TestPoolIdleReplacementKeepsNewOwner$' ./pkg/sshpool
go vet ./...
go tool cover -func=/tmp/knot-b06-cover.out
```

- 最终全量测试通过（session 7.615 秒、SFTP 8.828 秒、integration 2.109 秒）。
- 最终受影响包及完整协议链路的一次全量 race 通过（session 10.788 秒、SFTP 11.788 秒、pool 3.750 秒、core 1.156 秒、HTTP 1.988 秒、integration 2.975 秒），无 race 报告。
- 20 次定向 race 通过（session 8.604 秒、SFTP 11.387 秒、pool 1.512 秒、core 2.941 秒、HTTP 1.098 秒）；新增空闲替换回归另跑 20 次 race，通过（1.017 秒）。
- `go vet ./...`、`git diff --check` 通过；六目标以 `CGO_ENABLED=0 GOOS=... GOARCH=... go build -o /tmp/knot-b06-core-... ./cmd/core` 构建，Windows 使用 `.exe`。
- resourcepolicy 全部函数语句覆盖 100%；会话/Transfer 裁剪、待释放计数、subscriber 清理、worker 注册、maintenance 与 follow 取消的新函数 100%；SSH/SFTP Shutdown 分别 86.4% / 85.7%，其余未覆盖为底层关闭失败后的错误累积路径。pool stop/Shutdown/通知 100%，cleanup loop 83.3%、keepalive 95.7%。这些是函数级核查，不以整个旧业务包的总比例替代验收。
- 覆盖工件 `/tmp/knot-b06-cover.out`、`/tmp/knot-b06-cover-functions.txt`；未覆盖的旧 SSH/SFTP setup pipe/Agent 分支仍不等于平台已验收，本次不扩展密钥/Windows Agent。
- 空闲替换变异：Go overlay 恢复错误的 `notifyDisconnect(key)`，新测试确定失败，观察到 callbacks=1、replacement ref=0。工件 `/tmp/knot-b06-idle-callback-{overlay.json,mutation.go,mutation.log}`，工作区生产源码未被变异改写。

### 已明确的边界

历史在当前进程内保存；容量压力可提前裁剪，404 不代表执行成功。仍有实际 cleanup 的终态不裁剪，超过待释放容量时拒绝新增。detached 不自动 TTL 关闭，客户端负责显式断开。

用户提供的观察者回调可自己调用 Shutdown，因此不作为 Shutdown 的等待对象；最多一个调用和 256 条待处理通知，服务退出丢弃待处理服务通知，panic 隔离。业务 worker、取消回调、follow、backend、迟到 channel 与 pool 工作仍明确跟踪并等待。不可中断的第三方协议操作在预算内未完成时返回错误，由 transport 关闭最终解阻。

本轮没有运行外部 SSH 端点 smoke 或 macOS/Windows 原生用例，不改变 B07–B12 的剩余范围。

### B06 审阅修复后的验证

逐项结论与理由见 workspace `docs/running/B06-resource-lifecycle-plan.md` 的“审阅报告逐项核查与处理”。上节为初版验收；下列是本轮修复后的最终结果，仍以 `9e7fd0f` + 未提交工作区为基线，无新增依赖。

本轮修复无容量压力时反复排序及辅助 worker 重复裁剪 Transfer 的开销、exec 数量裁剪被 TTL 掩盖的测试、重复上限常量、pool 退出保留排队 observer、ActiveCount 判定重复，以及 core 的 GetClient/IncRef 取得窗口。连接 lease 返回前已经持有整条链路，释放绑定原条目且幂等；shared 拨号拥有独立的跳板引用，调用者取消后仍持有到实际拨号结束。shared 创建返回实际 revision publication key。仍保留兼容的 unowned GetClient / key-based IncRef/DecRef；core 资源均已迁移至 Acquire/lease。

R-1 采用报告建议 (b)：继续跟踪真实收尾，未采用超时提前归还引用。真实 SSH + gated stdin 的 `TestSessionTeardownTimeoutRetainsPoolOwner` 验证响应超时仍 ref=1、Shutdown 预算不足报错、实际收尾后 ref=0。访问/维护仍不裁剪正在收尾的终态；文档明确其可长期占用待释放额度。同 End 的字典序只是确定性并列规则，文档不承诺按创建顺序裁剪。空 Group 的期限错误保留，补充了共享预算不等于资源仍存活的说明。

最终执行目录 `knot-next/core`，所有 Go 命令沿用 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`；协议用例在已获准的可监听环境运行，没有跳过测试。

| 验证 | 最终结果 |
| --- | --- |
| 全量测试及覆盖 | `go test -count=1 -timeout=180s -coverpkg=./pkg/session,./pkg/sftp,./pkg/sshpool,./pkg/core,./internal/resourcepolicy -coverprofile=/tmp/knot-b06-review-cover.out ./...` 通过；session 7.563 秒、SFTP 8.735 秒、pool 2.689 秒、integration 1.865 秒 |
| 完整受影响包 race | `go test -race -count=1 -timeout=180s ./pkg/session ./pkg/sftp ./pkg/sshpool ./pkg/core ./internal/api/http ./tests/integration ./internal/resourcepolicy` 通过，无 race；session 9.609 秒、SFTP 10.187 秒、pool 3.764 秒、core 1.143 秒、HTTP 1.934 秒、integration 13.333 秒 |
| 20 次审阅/历史/收尾回归 | plan 中完整 `Test(Lease\|PoolShutdown\|SessionTeardown\|ExecHistory\|ExecPrune\|ExecShutdownKeeps\|HistoryWithinCapacity\|Terminal\|TransferHistory\|TransferRetention\|SessionCloseKeeps\|SFTPCloseKeeps)` 选择集，四包通过（session 2.402 秒、SFTP 1.525 秒、pool 1.712 秒、policy 1.014 秒） |
| 新增取得重试/实际 revision/拨号引用回归 | 最终 `go test -race -count=20 -timeout=60s -run 'Test(Lease\|PoolShutdown\|ReviewPoolCloseEndsContextAwarePrompt)' ./pkg/sshpool` 通过（2.029 秒）。包含 caller 取消后 dial 持跳板 ref=1、身份旋转不替换、预算错误及实际结束后原条目 ref=0 |
| 静态检查与构建 | `go vet ./...`、`git diff --check` 通过；六目标 `CGO_ENABLED=0 GOOS={linux,darwin,windows} GOARCH={amd64,arm64} go build -o /tmp/knot-b06-review-core-... ./cmd/core` 全部通过，Windows 为 `.exe` |
| 裁剪 benchmark | 4096 条未超限历史的 `BenchmarkHistoryWithinCapacity` 约 17.6 µs/op、0 B/op、0 allocs/op；仅测 policy，不代表服务请求整体零分配 |
| 变异验证 | 临时 overlay 删除 exact client 核对，`TestLeaseRetriesReplacementDuringAcquisition` 确定失败，报 `retry returned the closed client or disturbed the new owner`；工作区源码未变异 |

覆盖工件 `/tmp/knot-b06-review-cover.out`、`/tmp/knot-b06-review-cover-functions.txt`。resourcepolicy 全部函数、SSH/SFTP retention 除 Shutdown 外的新函数仍 100%；lease `Release/AcquireClientContext/AcquireClientContextWithPrompt/getClientContext/retainClient/retainPrefixLocked` 均为 100%。session/SFTP Shutdown 仍 86.4% / 85.7%，未覆盖底层 Close 硬错误累积；旧 pool `runCreation` 整体为 90.6%，不把新增 lease 函数的 100% 描述为所有旧拨号错误路径已完整覆盖。

变异工件为 `/tmp/knot-b06-review-lease-{overlay.json,mutation.go,mutation.log}`。Windows/macOS 原生运行和外部 SSH smoke 仍未执行；六目标构建仅作为编译证据。当前外部 observer 仍可长期阻塞，退出丢弃排队 observer 而不等待当前任意用户回调，业务/拨号/清理 worker 则独立等待。

### B06 复审 N-1–N-4 修复后的验证

逐项结论见 workspace `docs/running/B06-resource-lifecycle-plan.md` 的“复审 N-1–N-4 的处理”。原报告判断成立：N-1 的测试只把过期项放在末尾，不能证明原地压缩不改输入。本轮使 `Expired` 对所有输入保留内容和顺序，仅需要容量排序时复制存活项；prefix 释放加 once 和 defer；watcher 对 nil lease 防御；stop 显式禁止请求退出 disconnect 通知，与既定通知契约一致。pending cleanup 数量和最老待释放时长记录为 B07 候选，未提前增加 stats/event 字段。

沿用 Linux amd64、Go 1.27.1 与 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`；module 保持 Go 1.26.2，无新增依赖，未 Git 提交。真实协议用例在已获准的 loopback 环境执行，无 skip。

- 全量覆盖测试（沿用上节完整命令）通过：session 7.504 秒、SFTP 8.741 秒、pool 2.701 秒、integration 1.870 秒。结果另存 `/tmp/knot-b06-followup-cover.out`，函数级核查 `/tmp/knot-b06-followup-cover-functions.txt`；`Expired/retainPrefixLocked` 为 100%，原 watcher 整体 93.8%、runCreation 整体 91.2%，不声称所有旧分支覆盖完毕。
- 完整受影响包及协议集成 race（沿用上节完整命令）通过，无 race：session 9.650 秒、SFTP 10.205 秒、pool 3.772 秒、core 1.148 秒、HTTP 1.899 秒、integration 2.912 秒、policy 1.014 秒。
- `go test -race -count=20 -timeout=60s -run 'Test(History|PrefixRelease|CreationPanic|WatchInteractiveWithoutLease|Lease|PoolShutdown)' ./internal/resourcepolicy ./pkg/sshpool ./pkg/session` 通过（1.050 / 1.964 / 1.033 秒）。新增回归涵盖输入不变、20 个并发重复归还 prefix/idle 回收、异常 unwind 释放，以及无 lease 的有效 backend 实际收尾和终态诊断。
- 四个 Go overlay 独立恢复旧压缩、去掉 prefix once、去掉 deferred release、去掉 nil 检查，对应回归均以退出码 1 确定失败。工件 `/tmp/knot-b06-followup-{n1,n2-once,n2-defer,n3}.{json,go}`，未修改工作区生产源码。
- `go vet ./...`、`git diff --check`、六目标 `CGO_ENABLED=0 go build ./cmd/core` 通过。产物 `/tmp/knot-b06-followup-core-{linux,darwin,windows}-{amd64,arm64}`，Windows 带 `.exe`；Windows/macOS 原生运行及外部 SSH smoke 未执行。
- `BenchmarkHistoryWithinCapacity` 的 4096 条未超限历史约 14.9 µs/op、0 B/op、0 allocs/op。容量压力分支有意复制存活项以保留输入；benchmark 仅测 policy 的未超限扫描，不作为服务整体吞吐或零分配声明。

## B07 / B08：旧 TOML、加密和私钥 / 导入

2026-10-07 已实现：默认原位置 TOML 与旧 ENC/provider 复用、完整兼容字段保留、secret/API 分离、真实私钥元数据、attempt-only passphrase 贯通 SSH/SFTP/exec、SourcePath/cache 失效，以及额外 TOML 三策略导入、引用映射、revision/备份/原子替换和必要 bootstrap 恢复。公开行为见 [migration](migration.md)、[config](api/config.md)、[secrets](api/secrets.md)。

新增固定 fixture 由旧 Knot `e0b4d51eea6647e192371059381039b99fb301a2` 的 `Config.SaveToPath` / `EncryptWithKey` / `DeriveKey` / `NewState` 一次生成，测试读取提交的原密文。人工 key/身份与预期明文 hash 在 `pkg/config/testdata/legacy/README.md`；独立执行旧 `Config.LoadFromPath` 回读新 core 修改后的 TOML 成功。

验证包含读取/GET/status/plan/已有 provider 启动的文件集合与 hash 不变，forwards/客户端偏好/同步默认 alias 写回，未知字段拒绝有损写入，故障原件保持与跨 Service 并发 CRUD；三种导入策略、ID/alias 歧义与引用映射、两端 revision 冲突、重复 apply 和备份失败；bootstrap 子进程强制 kill 后恢复；真实 HTTP→SSH/SFTP 的缺/错/对口令、exec、SourcePath 替换/丢失、hashed/non-default-port/explicit trust 与变更 host key strict 拒绝。

Linux amd64、Go 1.27.1，使用 `GOWORK=off GOCACHE=/tmp/knot-plan-20261006-go-build`：

```bash
go test -count=1 -timeout=180s ./...
go test -race -count=1 -timeout=180s \
  ./pkg/config ./pkg/crypto ./pkg/session ./pkg/sftp ./pkg/sshpool \
  ./internal/api/http ./tests/integration

go test -race -count=5 -timeout=180s \
  -run 'Test(Legacy|Import|Explicit|EncryptedPrivate|PrivateKey|Passphrase|Bootstrap|ExistingLinux|PinnedLinux|UnknownTOML|ConfigFailures|ConcurrentTOML|ReferencedDeletion)' \
  ./pkg/config ./pkg/crypto ./tests/integration

go vet ./...
```

完整测试通过，受影响包一次完整 race 及新增用例五次 race 通过，无 race 报告；最终 metadata/presence/SourcePath/已有 provider 启动只读/recent_limit 默认值与整数校验检查另跑 config/crypto/paths race，通过。`git diff --check` 和 Linux/macOS/Windows × amd64/arm64 的六目标 `CGO_ENABLED=0 go build ./cmd/core` 通过。日志在 `/tmp/knot-b07-b08-{delivery-all,race,final-race,followup-race}.log`，构建产物在 `/tmp/knot-b07-b08-core-*`。

按本轮用户许可，macOS Keychain / Windows DPAPI 原生同账号互操作未运行；交叉构建不替代 B10/B12 平台验收。JSON 显式导入未实现，明确拒绝且保留来源。原 state.json/ID/trust 已复用；recent 成功回调在 B09 接入，额外 TOML 导入不自动搬移或合并历史和信任。SourcePath/fingerprint/encrypted 扩展可由新 core 保存，旧 loader 忽略字段，旧版不能使用 SourcePath-only key。forwards 仍 planned，客户端偏好未提前拆分。无新增依赖，旧项目未修改，未 Git 提交。

## B09–B11：客户端数据、Agent 与日志（2026-10-08）

本轮实现与 Linux 受控验收完成；完整证据见 [完成报告](../../../docs/running/B09-B11-completion-report.md)，开工前计划见 [实施计划](../../../docs/running/B09-B11-implementation-plan.md)。工作区基线 `2c6e1b82cfcf70463857c679717d1e81b7b240fd`，未提交。

最近使用沿用旧 state.json，真实目标成功节点更新、跳板/失败不更新；last_used/排序和 server_id 候选可供客户端使用。OSC7 有界旁路观察保留 PTY bytes；SFTP control 明确 cd/pause/resume，目录访问校验和源关闭失效可查询。Unix/Windows Agent 共用 context 拨号，认证连接临时持有、forwarding handler 按共享 client 持有，显式 setup 失败有 typed 状态。文件日志实际落盘、权限、脱敏、有限历史与明确关闭已接入。

Linux 全量测试、相关 race/跨包 cover（profile 合计 79.9%，重点函数单独审阅）、两组定向 20 次 race、vet 和六目标构建均通过。实际进程日志检测了 runtime LogPath、instance ID、最低生命周期记录、配置/导入失败和 token/password sentinel；Agent 验证真实远端签名与资源回收；follow 通过公开 API 和真实 SSH/SFTP 验证。

用户本轮明确不要求其他平台原生单元测试，Windows/macOS 代码逻辑审核与交叉构建完成，原生 Agent/凭据库/ACL/终端测试留到 CLI。用户明确授权后，两个真实 SSH 端点的显式 OSC7、strict SFTP follow、暂停/恢复与源关闭失效均通过；默认 shell 在 cd 后未观察到自动 OSC7，未修改登录脚本。首次审批拒绝及后续授权结果、接收 bytes/hash 见完成报告。不能把交叉编译宣称为其他平台原生运行通过。

自动 hook、客户端配置拆分、默认本地目录行为和日志 tail/follow 后置；本轮不宣布 B12 完整基线验收通过。
