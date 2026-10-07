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

## 容量、历史与关闭

活跃 SFTP session 最多 1024 个，累计关闭超过此数量后仍可新建。closed/failed/disconnected 历史在 backend、follow 和关联 worker 全部释放后可裁剪：从 `closed_at` 起保留 10 分钟，最多 1024 条，容量压力先删除最旧终态。GET/列表/订阅时清理，进程内每分钟也清理一次；重启或裁剪后 GET 返回 `404 NOT_FOUND`。仍有 worker 的终态不裁剪；释放中的 session 单独限为 1024 个，达到上限时新建返回 `409 CONFLICT`。这类记录代表实际收尾未完成；远端或底层 I/O 阻塞时可长期占用待释放额度。关闭响应超时后后台继续持有连接 lease，引用只在实际收尾结束后归还。

每个 session 最多 16 个 session events 订阅、16 个 transfer events 订阅；超限返回 `409 CONFLICT`。取消幂等且释放名额，关闭/失败/传输层断开会关闭并清空订阅。对保留终态订阅 session events，只发送快照后结束；transfer events 仍要求 session 非终态。

Transfer 最多 4096 个 queued/running/待收尾 worker。终态结果从 `completed_at` 起保留 10 分钟、最多 4096 条；容量压力可提前裁剪最旧已收尾任务。运行中的任务不会因 TTL 或数量裁剪。关闭 session 会先取消任务、释放 backend/follower；任务最终结果仍通过原 session ID + transfer ID 单项 GET 查询。单项 GET 的保留窗口独立于 session 历史，列表和重新订阅则要求所属 session 仍可查询。

连接取得时通过 lease 一并持有整条链路的引用，由实际 backend/worker owner 幂等释放，释放绑定原条目而不是可复用的 pool key。连接断开由每个 SFTP client 自行观察，不依赖事件通知是否送达。关闭单个 SFTP session 不主动关闭共享 SSH transport，其他 subsystem 可继续使用；整个 SSH transport 断开时，关联 session 分别收尾。Shutdown 停止新建、并行关闭资源并等待实际 worker/清理 loop，超出预算返回错误，不把 `state=closed` 当作已完全释放。

## 有口令私钥（B07）

SFTP 与 SSH 使用相同的 signer 构建和 attempt-only passphrase。`allow_auth_retry=true` 时，`GET /v1/sftp/{id}/challenges/auth` 的 `passphrase_required=true` 表示需要私钥口令；POST 到该地址可只传 `passphrase`。错误口令再次 challenge，正确口令完成 SSH 认证及 subsystem 打开后进入 open。`remember` 不持久化 passphrase。

## CWD follow 与 control（2026-10-08）

SFTP session 新增 `server_id`、`follow_state`、`follow_error`、`cwd_updated_at`。`current_dir` 是 SFTP 业务目录，SSH 的 OSC7 是独立来源状态；follow 通过 `follow_session_id` 显式关联，源与目标必须解析为相同服务器。多个 SSH 会话不会隐式选第一个。创建时 `current_dir=/` 是初始业务目录，`cwd_updated_at` 省略，表示尚未提交一次经访问校验的 cd/follow；来源目录要等 subsystem open 并校验成功后才提交。客户端结合 session state、`cwd_updated_at` 和 `follow_error` 判断是否已完成跟随，不把 connecting 阶段的 `/` 当成已验证的远端目录。

新增 `POST /v1/sftp/{id}/control`，成功返回 session view：

```json
{"op":"cd","path":"/var/log"}
```

| op | 行为 |
| --- | --- |
| `cd` | 校验目录存在且可列出后更新 SFTP CWD，相对路径按当前 SFTP CWD 解析；有有效 follow 时自动暂停。即使目录验证失败，也暂停 follow，避免用户操作被旧观察覆盖。 |
| `pause-follow` | 保留关联、最后目录和源订阅；后续观察不更新 SFTP CWD。 |
| `resume-follow` | 从关联 SSH session GET 最新目录并重新验证，成功后立即跟随；源未知目录时保持旧值。 |

无 follow 的暂停/恢复、已失效来源和未 open 的 session 返回 `409 CONFLICT`；无效 op/path、目录不可访问返回 `400 VALIDATION_FAILED`。不存在的 session 为 `404 NOT_FOUND`。

`follow_state` 为 `active`、`paused` 或 `invalid`；未关联时省略。每次跟随先绕过目录缓存执行实际读取权限校验。本地测试后端读取至多一个条目；远端目前使用库的完整 `ReadDirContext`（取消随服务退出，关闭客户端也终止 I/O），省去 Entry 构造、排序和分页。`Stat` 只能确认路径类型，无法证明可列出；现有库未公开目录句柄或限量读取接口，因此远端大目录的全量读取开销仍保留。失败保留最后可用目录，`follow_error=directory_unavailable`，发布 `sftp.cwd.follow_error`；有效的新观察/恢复成功清除错误。关闭源会停止 follower，保持最后目录，`follow_state=invalid` 并发布 `sftp.follow.invalidated`，不能恢复该关联。

相关事件：`sftp.cwd.changed`、`sftp.cwd.follow`、`sftp.cwd.follow_error`、`sftp.follow.paused`、`sftp.follow.invalidated`。事件有界，恢复以 session GET 为准。源停止时不会将目录重置到 `/`。本轮不自动注入 shell hook，也不使用客户端默认本地目录设置改变 core cwd。
