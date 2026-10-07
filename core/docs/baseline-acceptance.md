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
