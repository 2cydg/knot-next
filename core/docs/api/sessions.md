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
  "framework_error": "",
  "truncated": false,
  "started_at": "2026-05-15T12:00:00Z",
  "completed_at": "2026-05-15T12:00:01Z"
}
```

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

`host_key_policy` 允许值与 `POST /v1/sessions` 相同。

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
  "error": ""
}
```

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

## 常见错误

- `400 INVALID_JSON`
- `400 VALIDATION_FAILED`
- `400 WEBSOCKET_UPGRADE_FAILED`
- `404 NOT_FOUND`
- `409 CONFLICT`
- `500 INTERNAL_ERROR`

## 实现约束

- `attach` 只允许单个客户端附着；重复 attach 返回冲突
- `attach` 不区分 stdout/stderr 通道类型，二者都通过 binary frame 输出
- 当前没有独立的 `/cwd/events`；cwd 变化通过 session 事件中的 `path` 体现
- `session.exit` 是 attach WebSocket 的附加消息类型，不属于 `session.Event` 结构
- `session.exit` 由独立 goroutine 根据 attach stream 的 `Done` 信号发送，普通 session 事件由另一个 goroutine 转发；二者之间没有严格时序保证。客户端只能假定它出现在 attach 生命周期后段，不能假定它一定晚于所有 `session.Event`

## 客户端建议

- 如果只需要观察状态变化，用 `/events`，不要占用 `/attach`
- 如果已经 attach，就把 resize 和 detach 这类控制消息直接走 attach WebSocket，避免再发额外 HTTP 请求
- 客户端应同时处理 `session.error` 事件和 attach 流中的 `type=error` 消息，这两者来源不同
