# SFTP API

## 范围

本分册描述 SFTP 相关接口：

- SFTP session 生命周期
- 文件和目录操作
- 上传、下载、批量传输
- glob 匹配
- host key / auth challenge
- session 事件流
- transfer 事件流

## 资源模型

### SFTP Session

```json
{
  "id": "sftp_1",
  "server_ref": "prod",
  "alias": "prod",
  "state": "open",
  "backend": "ssh-sftp",
  "root": "/",
  "current_dir": "/var/log",
  "follow_session_id": "session_1",
  "host_key_policy": "ask",
  "host_key_pending": false,
  "auth_pending": false,
  "disconnect_cause": "",
  "events_url": "/v1/sftp/sftp_1/events",
  "transfer_events_url": "/v1/sftp/sftp_1/transfers/events",
  "created_at": "2026-05-15T12:00:00Z",
  "updated_at": "2026-05-15T12:00:10Z",
  "closed_at": null
}
```

当前常见 `state`：

- `connecting`
- `open`
- `host_key_pending`
- `auth_pending`
- `closed`
- `disconnected`
- `failed`

`backend` 表示该 SFTP session 的后端类型。真实 SSH/SFTP 会话创建时即为 `ssh-sftp`，在 `connecting` 阶段即可见；是否已经打开 subsystem 由 `state=open` 判定。内部测试环境也可能使用 `local-sandbox`。

### Entry

```json
{
  "name": "app.log",
  "path": "/var/log/app.log",
  "type": "file",
  "size": 123,
  "mode": "-rw-r--r--",
  "mod_time": "2026-05-15T12:00:00Z"
}
```

`type` 由实现推导，常见值为：

- `file`
- `dir`
- `symlink`
- 其他底层文件类型的字符串表示

### Transfer

```json
{
  "id": "transfer_2",
  "session_id": "sftp_1",
  "direction": "upload",
  "source": "/local/file.txt",
  "target": "/remote/file.txt",
  "state": "running",
  "bytes_total": 1234,
  "bytes_copied": 512,
  "files_total": 1,
  "files_done": 0,
  "current_path": "/remote/file.txt",
  "error": "",
  "items": [],
  "started_at": "2026-05-15T12:00:00Z",
  "completed_at": "0001-01-01T00:00:00Z"
}
```

当前常见 `state`：

- `queued`
- `running`
- `completed`
- `failed`
- `partial_failed`：批量项目部分成功、部分失败
- `canceled`

`queued`、`running` 为非终态；`completed`、`failed`、`partial_failed`、`canceled` 均为终态。
终态包含非零的 `completed_at`，一旦提交，不再被迟到进度、重复收尾或取消覆盖。
批量全失败为 `failed`，部分失败为 `partial_failed`；HTTP 202 仅表示任务已创建，客户端需要跟踪任务结果。

`items` 保存批量项目的状态与错误。取消时保留此前 completed/failed 项目的结果，当前中断项目和之后未开始的项目标为 canceled。
`bytes_copied` 可以在取消或失败时非零；`files_done` 只统计成功复制的文件。取消不承诺回滚已写入的文件、目录或数据。

### SFTP Event

```json
{
  "type": "sftp.session.opened",
  "session_id": "sftp_1",
  "state": "open",
  "path": "",
  "error": "",
  "transfer_id": "",
  "direction": "",
  "bytes_total": 0,
  "bytes_copied": 0,
  "files_total": 0,
  "files_done": 0,
  "current_path": "",
  "challenge": null,
  "time": "2026-05-15T12:00:00Z"
}
```

### Transfer Event

```json
{
  "type": "sftp.transfer.progress",
  "session_id": "sftp_1",
  "transfer_id": "transfer_2",
  "direction": "upload",
  "state": "running",
  "bytes_total": 1234,
  "bytes_copied": 512,
  "files_total": 1,
  "files_done": 0,
  "current_path": "/remote/file.txt",
  "error": "",
  "time": "2026-05-15T12:00:01Z"
}
```

## HTTP 接口

接口清单：

- `POST /v1/sftp`
- `GET /v1/sftp/{id}`
- `DELETE /v1/sftp/{id}`
- `GET /v1/sftp/{id}/files`
- `POST /v1/sftp/{id}/files`
- `HEAD /v1/sftp/{id}/files`
- `DELETE /v1/sftp/{id}/files`
- `POST /v1/sftp/{id}/dirs`
- `DELETE /v1/sftp/{id}/dirs`
- `POST /v1/sftp/{id}/rename`
- `POST /v1/sftp/{id}/upload`
- `POST /v1/sftp/{id}/download`
- `POST /v1/sftp/{id}/batch-upload`
- `POST /v1/sftp/{id}/batch-download`
- `GET /v1/sftp/{id}/matches`
- `GET /v1/sftp/{id}/challenges/host-key`
- `POST /v1/sftp/{id}/challenges/host-key`
- `GET /v1/sftp/{id}/challenges/auth`
- `POST /v1/sftp/{id}/challenges/auth`
- `GET /v1/sftp/{id}/transfers`
- `GET /v1/sftp/{id}/transfers/{transfer_id}`
- `DELETE /v1/sftp/{id}/transfers/{transfer_id}`

### POST `/v1/sftp`

创建 SFTP session。

请求体：

```json
{
  "server_ref": "prod",
  "alias": "prod",
  "follow_session_id": "session_1",
  "host_key_policy": "ask",
  "agent_socket": "/tmp/agent.sock",
  "allow_auth_retry": true
}
```

返回 `201` 和 `SFTP Session`。

创建是异步的，`201` 返回 `connecting` 资源，不表示 subsystem 已就绪。
客户端通过 GET 或 session 事件流等待 `open` 后再调用文件及传输接口；
尚未 open 时这些操作返回 `409 CONFLICT`。连接失败可通过 GET 查询 `failed` 终态。

`host_key_policy` 允许值与 `sessions.md` 中的 session 创建接口一致：

- `ask` 或空字符串
- `fail`
- `strict`
- `accept-new`
- `insecure-skip`

### GET `/v1/sftp/{id}`

返回单个 SFTP session。

### DELETE `/v1/sftp/{id}`

关闭 SFTP session。

### Files

#### GET `/v1/sftp/{id}/files`

默认执行目录列表。

query：

- `path`: 目标路径
- `show_hidden`: 是否显示隐藏文件
- `sort`: 排序字段
- `limit`: 分页大小
- `offset`: 偏移
- `cache`: 是否使用目录缓存，默认 `true`
- `stat`: 为 `true` 时执行 stat
- `op=stat`: 也可触发 stat

目录列表响应：

```json
{
  "entries": []
}
```

当 `stat=true` 或 `op=stat` 时，直接返回单个 `Entry`。

#### POST `/v1/sftp/{id}/files`

以请求体方式执行 stat。

请求体：

```json
{
  "path": "/var/log/app.log"
}
```

#### HEAD `/v1/sftp/{id}/files?path=...`

执行 stat。当前实现仍返回 JSON envelope，不是空响应头语义。

#### DELETE `/v1/sftp/{id}/files?path=...`

删除文件。

说明：

- 如果目标是目录，会返回 `400 VALIDATION_FAILED`

### Directories

#### POST `/v1/sftp/{id}/dirs`

创建目录。

请求体：

```json
{
  "path": "/var/log/app",
  "recursive": true
}
```

返回创建后的 `Entry`。

#### DELETE `/v1/sftp/{id}/dirs?path=...`

删除目录。

说明：

- 如果目标不是目录，会返回 `400 VALIDATION_FAILED`

### Rename

#### POST `/v1/sftp/{id}/rename`

请求体：

```json
{
  "old_path": "/var/log/app.log",
  "new_path": "/var/log/app-renamed.log"
}
```

返回新路径的 `Entry`。

### Single Transfer

#### POST `/v1/sftp/{id}/upload`

请求体：

```json
{
  "source": "/local/file.txt",
  "target": "/remote/file.txt",
  "overwrite": true,
  "recursive": false
}
```

返回 `202` 和 `Transfer`。

#### POST `/v1/sftp/{id}/download`

请求体同上，`source` 表示远端路径，`target` 表示本地路径。

返回 `202`。

### Batch Transfer

#### POST `/v1/sftp/{id}/batch-upload`

请求体：

```json
{
  "sources": ["/local/a.log", "/local/b.log"],
  "target": "/remote/logs",
  "overwrite": true,
  "recursive": false,
  "include_dirs": false
}
```

返回 `202` 和批量 `Transfer`。

#### POST `/v1/sftp/{id}/batch-download`

请求体结构相同。

### Glob

#### GET `/v1/sftp/{id}/matches?pattern=...`

query：

- `pattern`: 必填
- `include_dirs`: 是否包含目录
- `cache`: 是否使用缓存，默认 `true`

成功响应：

```json
{
  "entries": []
}
```

说明：

- 无匹配时当前实现返回 `404 NOT_FOUND`
- 这是当前错误映射导致的已知行为：空结果被映射为“未找到”而不是返回空 `entries`

### Challenge

#### GET `/v1/sftp/{id}/challenges/host-key`

#### POST `/v1/sftp/{id}/challenges/host-key`

请求体：

```json
{
  "accept": true,
  "abort": false,
  "remember": true
}
```

#### GET `/v1/sftp/{id}/challenges/auth`

#### POST `/v1/sftp/{id}/challenges/auth`

请求体：

```json
{
  "password": "secret",
  "key_id": "key_main",
  "remember": true
}
```

### Transfers

#### GET `/v1/sftp/{id}/transfers`

返回该 session 的 transfer 列表。

#### GET `/v1/sftp/{id}/transfers/{transfer_id}`

返回单个 transfer。

#### DELETE `/v1/sftp/{id}/transfers/{transfer_id}`

取消 transfer。

说明：

- 返回的是取消时的 transfer snapshot
- HTTP 200 表示取消请求已受理；实际状态转为 `canceled` 依赖后台 worker 异步收敛，继续 GET 查询直到终态
- 已进入终态的任务重复取消返回原结果，不改变 `completed_at`
- 取消与正常完成竞争时允许已完成的任务返回 completed；不会让已提交的终态翻转
- 使用错误的 session/transfer 组合查询或取消时返回 404
- 普通取消通过 worker 检查 context 收尾；远端永久不响应时，不能保证正在阻塞的网络 I/O 被立即打断
- 关闭 session 会先取消关联任务，再关闭 SFTP client；事件流可能先关闭，使用 GET 查询任务收尾结果

## WebSocket 接口

### GET `/v1/sftp/{id}/events`

订阅 SFTP session 事件。

首帧：

```json
{
  "type": "sftp.session.snapshot",
  "session": {}
}
```

之后每个 text frame 都是 `sftp.Event`。

典型事件：

- `sftp.session.created`
- `sftp.session.opened`
- `sftp.session.closed`
- `sftp.session.disconnected`
- `sftp.cwd.follow`
- `sftp.challenge.resolved`
- `sftp.error`

### GET `/v1/sftp/{id}/transfers/events`

订阅 transfer 事件。

首帧：

```json
{
  "type": "sftp.transfer.snapshot",
  "session_id": "sftp_1",
  "transfers": []
}
```

`transfers` 是本 session 当前保留的完整 Transfer 数组，按 `started_at` 升序排列；无任务时为 `[]`，不是 null。
正在运行的任务和已保留的终态任务都包含在首帧，因此完成后再订阅也能直接读取结果。
订阅注册与资源快照在同一临界区完成，随后发布的事件进入订阅通道。

之后每个 text frame 都是 `sftp.TransferEvent`。

典型事件：

- `sftp.transfer.queued`
- `sftp.transfer.started`
- `sftp.transfer.progress`
- `sftp.transfer.completed`
- `sftp.transfer.failed`
- `sftp.transfer.partial_failed`
- `sftp.transfer.canceled`

## 客户端建议

- 目录浏览优先使用 `GET /files`，单文件信息优先使用 `GET /files?stat=true`
- 长传输任务启动后，应改用 `/transfers` 或 `/transfers/events` 跟踪，不要轮询 upload/download 接口本身
- `matches` 无匹配时当前返回 `404`，不要把它当成空数组成功态
- 保存 POST 返回的 transfer ID；首帧查找该 ID，然后 GET 确认最新状态，任一终态均应结束等待
- 事件缓冲有界，慢消费者可能丢失进度或终态通知；GET 是任务结果依据，不能只等待某个事件必达
- WS 断线后 GET 或重订阅恢复原任务，不重复提交上传/下载；积压的旧 progress 不应覆盖已观察到的终态
- 快照仅包含当前进程内尚未清理的历史任务；core 重启或历史清理后 GET 可能返回 404，应报告结果不可查询
- 有界等待与恢复示例见 [Streaming](../streaming.md)

## 常见错误

- `400 INVALID_JSON`
- `400 VALIDATION_FAILED`
- `400 WEBSOCKET_UPGRADE_FAILED`
- `403 PERMISSION_DENIED`
- `404 NOT_FOUND`
- `409 CONFLICT`
- `500 INTERNAL_ERROR`

`403 PERMISSION_DENIED` 目前主要用于远端或本地后端返回的 permission denied 类错误。

## 实现约束

- `HEAD /files` 当前实现不是传统 HEAD 语义，而是返回 JSON body
- `GET /files` 同时承载 list 与 stat，两者通过 query 切换
- 当前没有单独的 list-all-sessions HTTP 接口，只有按 ID 的 session 资源和事件/transfer 子资源
