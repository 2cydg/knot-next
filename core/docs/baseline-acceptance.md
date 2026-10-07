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
