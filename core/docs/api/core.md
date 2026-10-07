# Core API

## 范围

本分册描述 core 级接口：

- 版本与健康检查
- 能力与运行时信息
- 状态摘要
- 全局事件 WebSocket
- 清理 SSH 连接池连接
- 关闭 daemon

## 资源模型

### Health

```json
{
  "status": "ok",
  "authenticated": true,
  "started_at": "2026-05-15T12:00:00Z",
  "checks": [
    {
      "name": "token",
      "status": "ok",
      "detail": ""
    }
  ]
}
```

### Capability

```json
{
  "name": "session",
  "status": "available",
  "risk": "LONG_RUNNING"
}
```

### Runtime Info

```json
{
  "version": "0.1.0-dev",
  "api_version": "v1",
  "pid": 12345,
  "port": 48652,
  "listen_addresses": ["127.0.0.1:48652", "[::1]:48652"],
  "token_path": "/path/to/token",
  "token_present": true,
  "log_path": "/path/to/core.log",
  "runtime_path": "/path/to/runtime.json",
  "config_dir": "/path/to/config",
  "state_dir": "/path/to/state",
  "started_at": "2026-05-15T12:00:00Z"
}
```

说明：

- 该结构同时用于本地 runtime 文件内容和 `GET /v1/runtime` 的响应 `data`
- `token_path` 指向由 `knot-core` 管理的本地 token 文件
- `token_present=true` 表示服务启动时已经成功加载或生成 token
- `runtime_path` 是当前 runtime 文件自身路径，便于 client 做诊断与重新发现

### Status

```json
{
  "uptime_seconds": 120,
  "active_sessions": 1,
  "active_sftp_sessions": 1,
  "running_transfers": 0,
  "active_forwards": 0,
  "running_tasks": 0,
  "ssh_pool": {
    "count": 1,
    "entries": []
  },
  "allocated_memory_bytes": 1234567
}
```

## HTTP 接口

接口清单：

- `GET /v1/version`
- `GET /v1/health`
- `GET /v1/capabilities`
- `GET /v1/runtime`
- `GET /v1/status`
- `POST /v1/connections/clear`
- `POST /v1/shutdown`

### GET `/v1/version`

返回服务版本信息。

响应 `data`：

```json
{
  "version": "0.1.0-dev",
  "api_version": "v1",
  "goos": "linux",
  "goarch": "amd64",
  "go_version": "go1.25.0",
  "commit": "",
  "build_time": "",
  "dirty": "",
  "compiler": "gc"
}
```

### GET `/v1/health`

返回健康检查结果。

说明：

- `status` 为 `ok` 或 `degraded`
- `checks` 当前包含 `token`、`runtime_file`、`listener`、`config`、`crypto`、`ssh_pool`

### GET `/v1/capabilities`

返回能力列表。

当前可能值包括：

- `core`
- `http_api`
- `token_auth`
- `runtime_discovery`
- `config`
- `secret`
- `session`
- `sftp`
- `forward`
- `broadcast`
- `sync`
- `archive`
- `update`
- `task`

`status` 当前实现使用：

- `available`
- `planned`

### GET `/v1/runtime`

返回运行时发现信息，用于本地客户端发现 daemon。

说明：

- 该接口本身仍然需要 token 认证
- 它适合已认证 client 查询当前 runtime 状态，不适合作为首次 bootstrap 的入口
- 首次 bootstrap 时，client 应先读取本地 runtime 文件，再读取 `token_path` 指向的 token 文件
- token 不属于 `config.toml` 配置模型，也不通过 config API 管理

### GET `/v1/status`

返回运行状态摘要。

重点字段：

- `active_sessions`
- `active_sftp_sessions`
- `running_transfers`
- `ssh_pool.count`
- `ssh_pool.entries`

### POST `/v1/connections/clear`

清理 SSH 连接池中的连接。

响应 `data`：

```json
{
  "closed": 2
}
```

说明：

- 返回值为本次关闭的 pool entry 数量
- 成功后会向全局事件流发布 `core.connections_cleared`

### POST `/v1/shutdown`

请求 core 关闭。

响应 `data`：

```json
{
  "shutdown": true
}
```

说明：

- 响应写回后异步执行 shutdown
- shutdown 过程中会尝试断开 session、关闭 sftp session、关闭 ssh pool

## WebSocket 接口

### GET `/v1/events`

订阅 core 全局事件。

首帧固定为：

```json
{
  "type": "core.snapshot",
  "api_version": "v1"
}
```

后续事件结构：

```json
{
  "type": "session.created",
  "resource": "session",
  "resource_id": "session_1",
  "level": "info",
  "time": "2026-05-15T12:00:00Z",
  "data": {}
}
```

事件对象字段：

- `type`: 事件类型
- `resource`: 资源类型，如 `session`、`sftp`、`sftp_transfer`、`ssh_pool`、`connections`、`core`
- `resource_id`: 可选资源 ID
- `level`: 当前实现通常为 `info`、`warning`、`error`
- `time`: 事件时间
- `data`: 事件附加数据

当前可观察到的典型事件包括：

- `session.created`
- `session.connected`
- `session.attached`
- `session.detached`
- `session.resized`
- `session.cwd`
- `session.challenge.resolved`
- `session.error`
- `sftp.session.opened`
- `sftp.session.created`
- `sftp.session.closed`
- `sftp.session.disconnected`
- `sftp.cwd.follow`
- `sftp.challenge.resolved`
- `sftp.error`
- `sftp.transfer.queued`
- `sftp.transfer.started`
- `sftp.transfer.progress`
- `sftp.transfer.completed`
- `sftp.transfer.failed`
- `sftp.transfer.canceled`
- `ssh_pool.disconnected`
- `core.connections_cleared`
- `core.shutdown_started`

## 通用错误

本分册接口常见错误：

- `401 AUTH_REQUIRED`
- `401 AUTH_FAILED`
- `403 PERMISSION_DENIED`
- `404 NOT_FOUND`
- `405 METHOD_NOT_ALLOWED`
- `400 WEBSOCKET_UPGRADE_FAILED`

## 客户端建议

- 本分册所有 HTTP 接口都只返回 `200` 成功态，不使用 `202`
- `/v1/status` 适合轮询；`/v1/events` 适合持续订阅
- `/v1/shutdown` 返回成功不代表进程已经完全退出，只表示已接受关闭请求

## 实现约束

- `/v1/events` 当前首帧只返回 API 版本，不返回完整资源 snapshot
- 全局事件 `data` 字段是事件类型相关结构，不是稳定 schema registry

## 长驻资源回收

SSH/SFTP 终态历史默认各保留 10 分钟、最多 1024 条；Transfer/exec 内部历史默认各保留 10 分钟、最多 4096 条，数量压力可提前裁剪完成时间最早的已收尾记录；完成时间相同时按 ID 字典序确定顺序，不承诺按创建顺序裁剪。活跃资源和仍有实际 worker 的终态不当作可裁剪历史。过期 GET 返回 404，客户端应停止等待并报告结果不可查询。

统一 Shutdown 会停止接收新资源，等待 session/exec/SFTP worker、连接创建、pool keepalive 和清理 loop，并关闭全局订阅；预算不足时明确返回收尾未完成错误，重复调用可以继续等待。普通配置保存不执行全局 Clear；凭据/链路变更只替换匹配的空闲连接，仍在使用的连接保持原 owner。`POST /v1/connections/clear` 明确主动断开当前 pooled 连接。

全局 `/v1/events` 最多 64 个订阅，超限或总线已关闭时返回 `409 EVENT_SUBSCRIPTION_UNAVAILABLE`。资源级订阅限额见 sessions/sftp 文档。

关闭响应的预算只限制调用者等待时间，不强制结束后台收尾。远端或底层 I/O 阻塞时，终态记录可能长期占用待释放额度，连接 lease 的引用直到实际收尾结束才归还；超时不会提前归还引用，也不会把终态裁剪成已释放资源。统一 Shutdown 同时关闭 pooled transport 以促使协议等待退出，仍未退出的工作按预算报告错误。多个资源类别共享同一预算，某类报告预算耗尽不必然表示该类仍有资源存活。
