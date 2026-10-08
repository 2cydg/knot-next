# Knot Core API v1

本目录存放 `knot-core` 当前已实现的 `v1` HTTP 与 WebSocket 接口文档。

这些文档遵循“代码即事实”的原则，内容以当前实现为准，范围对应：

- [core.md](./core.md)：核心状态、能力、运行时、全局事件、连接清理、关闭
- [config.md](./config.md)：配置读取、校验、迁移、settings、servers、proxies、keys、sync providers
- [secrets.md](./secrets.md)：secret 摘要、加密能力、secret 写入与删除
- [sessions.md](./sessions.md)：SSH session 资源、exec、challenge、attach、session 事件
- [sftp.md](./sftp.md)：SFTP session、文件操作、传输、challenge、SFTP 事件

未在本目录列出的能力，如 `forward`、`broadcast`、`sync`、`archive`、`update`、`task`，当前仍处于规划阶段；已实现且可用的端点均已列入上述分册。

## 覆盖矩阵

按功能分册划分的当前实现接口如下。

### core.md

- `GET /v1/version`
- `GET /v1/health`
- `GET /v1/capabilities`
- `GET /v1/runtime`
- `GET /v1/status`
- `GET /v1/events` WebSocket
- `POST /v1/connections/clear`
- `POST /v1/shutdown`

### config.md

- `GET /v1/config`
- `POST /v1/config/validate`
- `GET /v1/config/metadata`
- `GET /v1/config/migration`
- `GET /v1/config/migration/plan`
- `POST /v1/config/migration/apply`
- `GET /v1/config/settings`
- `GET /v1/config/settings/{key}`
- `PATCH /v1/config/settings/{key}`
- `DELETE /v1/config/settings/{key}`
- `GET /v1/config/servers`
- `POST /v1/config/servers`
- `GET /v1/config/servers/{id}`
- `PUT /v1/config/servers/{id}`
- `DELETE /v1/config/servers/{id}`
- `GET /v1/config/servers/resolve?ref=...`
- `GET /v1/config/proxies`
- `POST /v1/config/proxies`
- `GET /v1/config/proxies/{id}`
- `PUT /v1/config/proxies/{id}`
- `DELETE /v1/config/proxies/{id}`
- `GET /v1/config/keys`
- `POST /v1/config/keys`
- `GET /v1/config/keys/{id}`
- `PUT /v1/config/keys/{id}`
- `DELETE /v1/config/keys/{id}`
- `GET /v1/config/sync-providers`
- `POST /v1/config/sync-providers`
- `GET /v1/config/sync-providers/{id}`
- `PUT /v1/config/sync-providers/{id}`
- `DELETE /v1/config/sync-providers/{id}`
- `PUT /v1/config/sync-providers/{id}/default`
- `DELETE /v1/config/sync-providers/{id}/default`

### secrets.md

- `GET /v1/secrets`
- `GET /v1/secrets/crypto`
- `PUT /v1/secrets/servers/{id}/password`
- `DELETE /v1/secrets/servers/{id}/password`
- `PUT /v1/secrets/proxies/{id}/password`
- `DELETE /v1/secrets/proxies/{id}/password`
- `PUT /v1/secrets/keys/{id}/private`
- `DELETE /v1/secrets/keys/{id}/private`
- `PUT /v1/secrets/sync/password`
- `DELETE /v1/secrets/sync/password`
- `PUT /v1/secrets/sync-providers/{id}/password`
- `DELETE /v1/secrets/sync-providers/{id}/password`
- `PUT /v1/secrets/sync-providers/{id}/s3-credentials`
- `DELETE /v1/secrets/sync-providers/{id}/s3-credentials`

### sessions.md

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
- `GET /v1/sessions/{id}/attach` WebSocket
- `GET /v1/sessions/{id}/events` WebSocket

### sftp.md

- `POST /v1/sftp`
- `GET /v1/sftp/{id}`
- `DELETE /v1/sftp/{id}`
- `GET /v1/sftp/{id}/events` WebSocket
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
- `GET /v1/sftp/{id}/transfers/events` WebSocket

## 约定

### 1. 版本

- 当前公开 API 版本为 `v1`
- 所有 HTTP 路径都以 `/v1` 开头
- 所有 WebSocket 路径也属于 `v1` 协议空间

### 2. 本地发现与初始化

`knot-core` 的本地 client bootstrap 依赖两份本地文件：

- runtime 文件：记录 daemon 的监听地址、端口、token 文件路径、日志路径等运行时信息
- token 文件：保存 HTTP / WebSocket 认证所需的 bearer token

约束：

- token 不是 `config.toml` 的用户配置项
- token 不通过 config API 创建、修改或下发
- token 由 `knot-core` 在启动时自动加载；不存在时自动生成并写入本地 token 文件
- client 不负责生成 token，只负责发现并读取 token

默认路径规则：

- 所有平台沿用旧 Knot 路径：`config_dir = {XDG_CONFIG_HOME}/knot`，未设置时为 `~/.config/knot`。
- `state_dir = {XDG_STATE_HOME}/knot`，未设置时为 `~/.local/state/knot`。
- `runtime_file = {state_dir}/runtime/core.json`
- `token_file = {state_dir}/runtime/token`
- `config_file = {config_dir}/config.toml`
- 显式启动参数可覆盖目录；客户端读取实际 runtime 内的 `token_path`，不猜测平台目录。

推荐的首次连接顺序：

1. client 定位 `runtime_file`
2. 若 runtime 文件不存在，则启动或唤起 `knot-core`
3. `knot-core` 启动后自动生成或加载 token，并写出 runtime 文件
4. client 读取 runtime 文件，获得 `listen_addresses`、`port`、`token_path`
5. client 从 `token_path` 读取 token
6. client 对后续所有 HTTP / WebSocket 请求携带 `Authorization: Bearer <token>`

注意：

- `/v1/runtime` 也需要 token 认证，不能作为“第一次无认证发现”接口
- 第一次 bootstrap 必须依赖本地 runtime 文件，而不是先调用 HTTP API

### 3. 认证

所有 HTTP 和 WebSocket 请求都必须通过 token 认证。

支持两种携带方式：

- `Authorization: Bearer <token>`
- `X-Knot-Token: <token>`

未携带 token 时返回 `401 AUTH_REQUIRED`。token 错误时返回 `401 AUTH_FAILED`。

### 4. Origin 校验

- 没有 `Origin` 头时默认允许
- 带 `Origin` 头时，必须在服务端允许列表中
- 不允许的来源返回 `403 PERMISSION_DENIED`

### 5. HTTP 成功响应

除 WebSocket 握手外，HTTP 成功响应统一为 JSON envelope：

```json
{
  "data": {},
  "meta": {
    "timestamp": "2026-05-15T12:00:00Z"
  }
}
```

说明：

- `data` 为业务数据
- `meta.timestamp` 为服务端 UTC 时间

### 6. HTTP 错误响应

HTTP 错误统一返回：

```json
{
  "error": {
    "code": "VALIDATION_FAILED",
    "message": "request validation failed",
    "details": {},
    "retryable": false,
    "risk": "READ_ONLY",
    "resource": "config/servers",
    "suggested_action": "optional human hint"
  },
  "meta": {
    "timestamp": "2026-05-15T12:00:00Z"
  }
}
```

`details` 和 `suggested_action` 仅在部分错误中出现。

### 7. 风险级别

当前实现使用以下 `risk` 值：

- `READ_ONLY`
- `LOCAL_MUTATION`
- `REMOTE_READ`
- `REMOTE_MUTATION`
- `LONG_RUNNING`
- `DESTRUCTIVE`

### 8. JSON 解析规则

- 请求体必须是单个 JSON 对象
- 不能包含未知字段
- 非法 JSON 返回 `400 INVALID_JSON`

### 9. WebSocket 约定

- 通过标准 WebSocket Upgrade 建立连接
- 服务端发送 text frame 承载结构化事件
- SSH attach 的 stdout/stderr 使用 binary frame
- 客户端可以发送 ping，服务端会响应 pong
- 服务端也会周期性发送 ping 保活

### 10. 文档书写规范

本目录各分册统一使用以下结构：

1. 功能范围
2. 资源模型
3. HTTP 接口
4. WebSocket 接口
5. 备注或实现限制

如某接口存在已实现但仍偏底层的行为，文档会明确标记“实现约束”而不是把它伪装成稳定抽象。

### 11. 使用建议

- 读取能力、状态、配置时优先使用 HTTP
- 需要交互字节流或持续事件订阅时使用 WebSocket
- 处理写操作时，客户端应同时根据 HTTP 状态码和错误 envelope 的 `error.code` 做分支
- 配置与 secret 必须分开处理，不要尝试通过 config endpoint 直接提交 secret 明文

### 基线客户端交接

可运行的公开 API 工作流见 [示例说明](../../examples/baseline/README.md)；正式客户端接入见 [CLI 交接清单](../cli-handoff.md)。HTTP JSON 请求最多 1 MiB，严格拒绝未知字段、超限及多对象；WS 完整数据消息最多 1 MiB，支持 continuation 和片间控制帧，所有发送最多等待 5 秒。
