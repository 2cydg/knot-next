# Sessions API

## 范围

本分册描述 SSH session 相关接口：

- session 创建、查询、关闭
- session 过滤
- exec
- control
- host key challenge
- auth challenge
- attach WebSocket
- session events WebSocket

## 资源模型

### Session Resource

```json
{
  "id": "session_1",
  "server_ref": "prod",
  "alias": "prod",
  "state": "attached",
  "term": "xterm-256color",
  "rows": 40,
  "cols": 120,
  "env": {
    "LANG": "en_US.UTF-8"
  },
  "forward_agent": false,
  "host_key_policy": "ask",
  "current_dir": "/var/log",
  "cwd_updated_at": "2026-05-15T12:00:00Z",
  "attached": true,
  "started_at": "2026-05-15T12:00:00Z",
  "updated_at": "2026-05-15T12:00:10Z",
  "exited_at": null,
  "exit_code": null,
  "framework_error": "",
  "attach_url": "/v1/sessions/session_1/attach",
  "events_url": "/v1/sessions/session_1/events",
  "host_key_pending": false,
  "auth_pending": false,
  "disconnect_cause": ""
}
```

当前常见 `state`：

- `connecting`
- `attached`
- `detached`
- `host_key_pending`
- `auth_pending`
- `closed`
- `failed`

### Exec

```json
{
  "id": "exec_2",
  "server_ref": "prod",
  "command": "uptime",
  "state": "completed",
  "exit_code": 0,
  "stdout": " ... ",
  "stderr": "",
  "truncated": false,
  "started_at": "2026-05-15T12:00:00Z",
  "completed_at": "2026-05-15T12:00:01Z"
}
```

成功结果省略 `framework_error`、`framework_code` 和 `cleanup_error`。失败及未完成收尾的示例：

```json
{
  "id": "exec_3",
  "server_ref": "prod",
  "command": "sleep 60",
  "state": "failed",
  "exit_code": -1,
  "stdout": "",
  "stderr": "",
  "framework_code": "timeout",
  "framework_error": "context deadline exceeded",
  "cleanup_error": "cleanup_timeout",
  "truncated": false,
  "started_at": "2026-05-15T12:00:00Z",
  "completed_at": "2026-05-15T12:00:06Z"
}
```

exec 的 `id` 用于诊断和关联本次同步调用。目前没有按 exec ID 查询结果的 GET 接口；`GET /v1/sessions/{id}` 查询的是交互 SSH session，客户端应保存同步返回结果。

### Challenge

```json
{
  "session_id": "session_1",
  "type": "host_key",
  "pending": true,
  "prompt": "Do you trust this host key?",
  "fingerprint": "SHA256:...",
  "host": "10.0.0.1",
  "key_type": "ssh-ed25519",
  "risk": "unknown_host",
  "server_alias": "prod",
  "failed_method": "",
  "allowed_methods": [],
  "retry_count": 0,
  "created_at": "2026-05-15T12:00:00Z",
  "updated_at": "2026-05-15T12:00:00Z"
}
```

### Session Event

```json
{
  "type": "session.resized",
  "session_id": "session_1",
  "state": "attached",
  "rows": 40,
  "cols": 120,
  "path": "",
  "exit_code": null,
  "error": "",
  "challenge": null,
  "time": "2026-05-15T12:00:10Z"
}
```

## HTTP 接口

接口清单：

- `GET /v1/sessions`
- `POST /v1/sessions`
- `POST /v1/sessions/exec`
- `GET /v1/sessions/{id}`
- `DELETE /v1/sessions/{id}`
- `POST /v1/sessions/{id}/control`
- `GET /v1/sessions/{id}/challenges/host-key`
- `POST /v1/sessions/{id}/challenges/host-key`
- `GET /v1/sessions/{id}/challenges/auth`
- `POST /v1/sessions/{id}/challenges/auth`

### GET `/v1/sessions`

返回 session 列表。

支持 query：

- `server_ref`
- `alias`
- `state`

过滤规则：

- `server_ref` 同时匹配 `session.server_ref` 和 `session.alias`
- `alias` 只匹配 `session.alias`
- `state` 精确匹配状态

### POST `/v1/sessions`

创建交互式 SSH session。

请求体：

```json
{
  "server_ref": "prod",
  "alias": "prod",
  "term": "xterm-256color",
  "rows": 40,
  "cols": 120,
  "env": {
    "LANG": "en_US.UTF-8"
  },
  "forward_agent": false,
  "host_key_policy": "ask",
  "allow_auth_retry": true
}
```

返回 `201` 和 `Session Resource`。

`host_key_policy` 当前允许值：

- `ask` 或空字符串：遇到未知或变更主机密钥时进入 challenge 流
- `fail`: 直接拒绝未知或不匹配的主机密钥
- `strict`: 严格校验已知主机密钥
- `accept-new`: 自动接受首次见到的主机密钥
- `insecure-skip`: 跳过主机密钥校验

### POST `/v1/sessions/exec`

执行非交互命令。

请求体：

```json
{
  "server_ref": "prod",
  "command": "uptime",
  "timeout_ms": 5000,
  "host_key_policy": "ask"
}
```

返回 `Exec`。

此接口同步返回，不创建异步任务。远端正常退出（包括非零退出码）返回 HTTP 200、`state=completed`；连接或执行框架失败返回 HTTP 200、`state=failed`、`exit_code=-1`。无效参数返回 HTTP 400，服务器引用不存在返回 404，服务正在退出返回 409。

`timeout_ms` 必须为可转换为 Go duration 的非负整数（最大 `9223372036854`）。省略或 0 表示没有总操作期限；HTTP 请求取消/断开和 core shutdown 仍会取消执行。正值从受理 exec 开始计时，覆盖配置读取、连接、channel 创建、命令请求及执行；同步配置文件读取不能在一次系统调用中被强制打断。网络阶段有独立期限，channel 创建最多等待 15 秒，结束时另有最多约 5 秒的收尾预算，因此响应耗时可以略大于 `timeout_ms`。长命令的客户端 HTTP timeout 应相应增加。

取消时尝试发送 `KILL`，随后关闭本次 SSH channel，并等待执行和双流读取完成；不关闭其他会话共享的 SSH client。远端后台进程是否停止取决于服务器，不承诺杀死脱离 SSH channel 的进程。HTTP 已断开时没有结果响应可接收。

命令结果与取消同时可见时，优先返回已经完成并排空输出的命令结果；尚未发布执行结果时取消生效。

每流独立保留前 512 KiB，继续消费超出的字节并设置合并的 `truncated=true`；不在 stdout/stderr 增加截断提示。正常完成时双流已经排空；取消时保留本地实际读到的部分。两流内部顺序保留，不承诺跨流顺序；JSON 字符串不用于无效 UTF-8 的原始二进制传输。

`framework_code` 是稳定机器码，成功时省略；`framework_error` 是脱敏说明，不应据英文文本判定行为：

| `framework_code` | 含义 |
| --- | --- |
| `timeout` | 总期限、连接阶段或 channel 创建期限到期 |
| `canceled` | caller/HTTP 请求取消 |
| `service_shutdown` | core 正在退出 |
| `host_key_verification_failed` | 主机密钥未知、变更或不符合信任策略 |
| `authentication_failed` | 缺少凭据或 SSH 认证被拒绝 |
| `exit_status_missing` | 服务端关闭 channel 但没有报告退出结果 |
| `ssh_error` | 其他连接、channel 或执行协议错误 |
| `cleanup_timeout` | 没有其他失败原因，但收尾未正常完成 |

`cleanup_error` 成功收尾时省略；`cleanup_timeout` 表示执行/信号/关闭 worker 在预算内未结束，`channel_close_failed` 表示 channel 关闭失败，`channel_open_pending` 表示被取消的 channel open 尚无服务端确认。第三方 SSH 库不能撤回未确认的 open，也不能本地强制结束不回应 channel close 的协议等待。此时调用返回失败或保留取消原因，后台仍跟踪实际 worker 并持有 pool ref，直到迟到 channel 被关闭或共享 transport 退出；core shutdown 等待超限会报告错误。这些字段不表示资源已经回收，完整的极端网络回收验收仍待补齐。

`host_key_policy` 支持省略、`ask`、`fail`、`strict`、`accept-new`、`insecure-skip`；`ask` 在内部规范化为默认策略。exec 没有交互 challenge 通道，未知或变更主机密钥直接失败；客户端应先通过 SSH session challenge 建立信任并配置凭据，再执行 exec。策略文档与 `POST /v1/sessions` 一致，但本同步接口不会等待用户答复。

### GET `/v1/sessions/{id}`

返回单个 session。

### DELETE `/v1/sessions/{id}`

断开 session。

返回断开后的 session 资源。

### POST `/v1/sessions/{id}/control`

发送控制动作。

请求体：

```json
{
  "type": "resize",
  "rows": 40,
  "cols": 120
}
```

支持 `type`：

- `resize`
- `detach`
- `close_stdin`
- `signal`
- `disconnect`

各类型附加字段：

- `resize`: `rows`, `cols`
- `signal`: `signal`

支持的 `signal`：

- `HUP`
- `INT`
- `KILL`
- `TERM`
- `USR1`
- `USR2`

### GET `/v1/sessions/{id}/challenges/host-key`

查询 host key challenge。

没有 pending challenge 时返回：

```json
{
  "session_id": "session_1",
  "type": "host_key",
  "pending": false
}
```

### POST `/v1/sessions/{id}/challenges/host-key`

响应 host key challenge。

请求体：

```json
{
  "accept": true,
  "abort": false,
  "remember": true
}
```

### GET `/v1/sessions/{id}/challenges/auth`

查询 auth challenge。

### POST `/v1/sessions/{id}/challenges/auth`

响应 auth challenge。

请求体：

```json
{
  "password": "secret",
  "key_id": "key_main",
  "remember": true,
  "passphrase": "optional"
}
```

说明：

- `ChallengeResponse` 是通用结构，实际使用字段由 challenge 类型决定

## WebSocket 接口

### GET `/v1/sessions/{id}/attach`

交互式 attach WebSocket。

服务端首批 text frame：

1.

```json
{
  "type": "session.snapshot",
  "session": {}
}
```

2.

```json
{
  "type": "session.attached",
  "session_id": "session_1",
  "state": "attached"
}
```

随后数据流规则：

- 服务端 `binary frame`：stdout/stderr 原始字节流
- 服务端 `text frame`：session 事件，例如 `session.resized`、`session.challenge.resolved`、`session.exit`
- 客户端 `binary frame`：写入 stdin
- 客户端 `text frame`：按 `ControlRequest` JSON 解析，等价于调用 `/control`

控制消息示例：

```json
{
  "type": "resize",
  "rows": 50,
  "cols": 160
}
```

attach 失败时，服务端直接写入 text frame：

```json
{
  "type": "error",
  "code": "ATTACH_FAILED",
  "message": "..."
}
```

服务端在升级 WebSocket **之前**就先校验并占用附着权。因此以下情况直接返回普通 HTTP 错误，不会升级连接：

- session 不存在：`404 NOT_FOUND`
- session 已 `closed`/`failed`、已被其他客户端附着、或 PTY backend 尚未就绪：`409 CONFLICT`

升级之后才发生的失败（例如升级成功后事件订阅失败）才使用上面的 `ATTACH_FAILED` text frame。被拒绝的请求不会留下附着状态，第一个客户端不受影响。

control 失败时，服务端写入：

```json
{
  "type": "error",
  "code": "CONTROL_FAILED",
  "message": "..."
}
```

session 结束时，服务端会额外发送：

```json
{
  "type": "session.exit",
  "session_id": "session_1",
  "exit_code": 0,
  "error": "",
  "disconnect_cause": ""
}
```

字段语义：

- `exit_code`：远端退出码。`0` 是明确的成功；非零保留远端原值（远端被信号终止时为 `128+signum`，例如 `SIGKILL` 为 `137`）；**没有收到 exit-status 时为 `null`**，不能当作成功。
- `error`：框架级失败信息（网络错误、缺少 exit-status 等）。远端正常退出或非零退出时为空。
- `disconnect_cause`：机器可读的结束原因，可能取值 `exit_status_missing`、`network_error`、`client_disconnected`、`pool_disconnected`、`remote_signal`；远端正常退出时为空。连接被对端或连接池销毁时有多种原因同时成立，字段记录最先被观察到的那一个。

`session.exit` 的退出码与同一时刻 `GET /v1/sessions/{id}` 的 `exit_code` 一定一致：两者来自同一个只读结果快照。

attach WebSocket 帧方向总结：

- client -> server binary: stdin bytes
- client -> server text: `ControlRequest`
- server -> client binary: stdout/stderr bytes
- server -> client text: snapshot、事件、错误、exit 附加消息

### GET `/v1/sessions/{id}/events`

订阅 session 事件。

首帧：

```json
{
  "type": "session.snapshot",
  "session": {}
}
```

之后每个 text frame 都是 `session.Event` JSON。

当前常见事件：

- `session.created`
- `session.connected`
- `session.attached`
- `session.detached`
- `session.resized`
- `session.cwd`
- `session.challenge.resolved`
- `session.error`

## 容量、历史与订阅回收

- 活跃 SSH 会话最多 1024 个；connecting、challenge、connected、attached 和 detached 都占用活跃容量。累计创建/关闭次数不影响新建。
- 已释放资源的 closed/failed 历史从 `exited_at` 起保留 10 分钟，最多 1024 条；数量压力先裁剪最旧终态。GET 触发过期清理，进程内每分钟也清理一次。裁剪或重启后 GET 返回 `404 NOT_FOUND`，不能据此推断远端执行成功。
- 关闭结果先提交，连接/附件/output pump 的实际收尾随后完成。仍有 worker 的终态不裁剪；释放中的资源单独限为 1024 个，达到该上限时新建返回 `409 CONFLICT`，避免异常远端导致待释放对象无限累积。关闭调用超时后后台继续持有连接 lease；远端或底层 I/O 长时间阻塞时，该记录可能长期占用待释放额度，直到实际收尾结束。超时不表示引用已经归还。detached 保持活跃，需客户端显式关闭。
- 每个 session 最多 16 个事件订阅（attach 的内部事件订阅也计入），最多 8 个 CWD follower。超限返回 `409 CONFLICT`；取消可重复调用并释放名额。
- session 进入终态时关闭事件/CWD 订阅。对保留终态再次订阅 session events，只发送快照后结束。慢消费者可能丢事件，应使用 GET 确认结果。
- exec 最多 4096 个并发执行/待收尾操作；内部终态历史保留 10 分钟、最多 4096 条。待收尾操作不因历史裁剪丢失诊断结果。

## 常见错误

- `400 INVALID_JSON`
- `400 VALIDATION_FAILED`
- `400 WEBSOCKET_UPGRADE_FAILED`
- `404 NOT_FOUND`
- `409 CONFLICT`
- `500 INTERNAL_ERROR`

## 实现约束

- `attach` 只允许单个客户端附着；重复 attach 在升级前返回 `409 CONFLICT`，且不影响已附着客户端
- `attach` 不区分 stdout/stderr 通道类型，二者都通过 binary frame 输出
- 客户端 binary frame 是 PTY 原始字节，服务端不解释、不改写换行、不做 ANSI/编码处理；允许重新分帧，但字节内容不变。同一流（stdout 或 stderr）内部顺序保持；两个流之间不承诺全局顺序，PTY 合流按真实到达顺序
- 服务端在正常结束时会**先发完已收到的输出字节，再发送唯一的 `session.exit`，随后关闭连接**；客户端可以据此认为 `session.exit` 之后不会再有输出
- 未附着期间产生的输出保存在**有界**缓冲（上限 64 KiB）中，在下一个客户端 attach 时先补发。已发送给前一个 attach 的字节不会重放，因此重新 attach 不会看到重复输出；超过上限时丢弃最旧的字节，并通过 `session.attached` 之后的一个 text frame 告知：
  ```json
  {"type": "session.attach.truncated", "session_id": "session_1", "detail": "pre-attach output exceeded the retained backlog"}
  ```
- 慢客户端不会被无限缓冲：服务端有界队列写满或写入超时后，attach 会被明确结束并关闭连接（客户端此时可能收不到错误帧，因为它已停止读取）。session 本身保持存活并可重新 attach
- 输入写入按 session 串行处理。detach 丢弃旧 attachment 尚未发送的队列；已经进入 SSH 传输层的输入无法撤回，可能在 detach 后送达。远端停止读取时，新 attachment 可建立，但其输入等待同一 backend 写入完成，队列保持有界，不会为每次重新附着新增阻塞写入者。
- disconnect 会先记录资源终态，再释放 backend；关闭超时或失败通过错误响应返回，不能把 `state=closed` 单独当作物理资源全部释放的证明。
- resize 只在 `rows`/`cols` 均处于 1..1000 时下发真实 `window-change`；非法值返回 `400 VALIDATION_FAILED`（HTTP）或 `CONTROL_FAILED`（WS），且不发送到远端。`close_stdin` 可重复发送且幂等；`signal` 成功时不产生额外事件，可通过远端行为观察
- 当前没有独立的 `/cwd/events`；cwd 变化通过 session 事件中的 `path` 体现
- `session.exit` 是 attach WebSocket 的附加消息类型，不属于 `session.Event` 结构
- `session.exit` 在所有输出帧之后由同一个发送队列发出，之后连接关闭；其他 session 事件仍由独立订阅转发，不保证与 `session.exit` 的先后关系（只能假定 `session.exit` 是最后一条消息）

## 客户端建议

- 如果只需要观察状态变化，用 `/events`，不要占用 `/attach`
- 如果已经 attach，就把 resize 和 detach 这类控制消息直接走 attach WebSocket，避免再发额外 HTTP 请求
- 客户端应同时处理 `session.error` 事件和 attach 流中的 `type=error` 消息，这两者来源不同

## 有口令私钥（B07）

`allow_auth_retry=true` 时，缺少 passphrase 进入 `auth_pending`，challenge 带 `failed_method="key"` 和 `passphrase_required=true`；响应可只带 `passphrase`，默认使用当前 key_id。错误口令重新 challenge，正确口令进入真实 signer / SSH 认证。`remember` 不保存 passphrase，仅保存既有允许的 password / key_id 选择。

同步 `POST /v1/sessions/exec` 可传 attempt-only `passphrase`；缺失口令结果为 `framework_code="passphrase_required"`。它不进入 exec 历史、普通 GET、事件或 TOML。SourcePath 在每次建立连接时由 core 读取；文件内容和本次口令参与 pool 身份摘要，避免复用旧身份。
