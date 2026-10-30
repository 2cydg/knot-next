# Knot Core API v1

本目录存放 `knot-core` 当前已实现的 `v1` HTTP 与 WebSocket 接口文档。

这些文档遵循“代码即事实”的原则，内容以当前实现为准，范围对应：

- [core.md](./core.md)：核心状态、能力、运行时、全局事件、连接清理、关闭
- [config.md](./config.md)：配置读取、校验、迁移、settings、servers、proxies、keys、sync providers
- [secrets.md](./secrets.md)：secret 摘要、加密能力、secret 写入与删除
- [sessions.md](./sessions.md)：SSH session 资源、exec、challenge、attach、session 事件
- [sftp.md](./sftp.md)：SFTP session、文件操作、传输、challenge、SFTP 事件
- [connections.md](./connections.md)：独立连接身份、完整路由取得与候选
- [proxy-tests.md](./proxy-tests.md)：草稿/已存代理的只读探测
- [forwards.md](./forwards.md)：持久定义、临时/自动 L/R/D 转发、运行状态与事件

未在本目录列出的能力，如 `broadcast`、`sync`、`archive`、`update`、`task`，当前仍处于规划阶段；已实现且可用的端点均已列入上述分册。

## 覆盖矩阵

按功能分册划分的当前实现接口如下。

### core.md

- `GET /v1/version`
- `GET /v1/health`
- `GET /v1/capabilities`
- `GET /v1/runtime`
- `GET /v1/status`
- `GET /v1/events` WebSocket
- `GET /v1/connections`
- `GET /v1/connections/{id}`
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
- `GET /v1/config/servers/{id}/forwards`
- `PUT /v1/config/servers/{id}/forwards`
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
- `POST /v1/sftp/{id}/control`
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

### proxy-tests.md / forwards.md

- `POST /v1/proxy-tests`
- `GET /v1/forwards`
- `POST /v1/forwards`
- `GET /v1/forwards/{id}`
- `DELETE /v1/forwards/{id}`
- `POST /v1/forwards/{id}/control`
- `GET /v1/forwards/{id}/events` WebSocket

## 约定

### 1. 版本

- 当前公开 API 版本为 `v1`
- 所有 HTTP 路径都以 `/v1` 开头
- 所有 WebSocket 路径也属于 `v1` 协议空间

### 2. 配置与客户端初始化

`knot-core init` 在当前目录生成 `config.toml`，不启动服务；写入默认端口 17898 和随机 32 位数字/大小写字母 Secret，文件权限 0600，已有配置不会覆盖。

```toml
[service]
port = 17898
secret = "" # 空字符串表示免 Secret 模式
allowed_origins = []
```

启动使用 `knot-core --config <文件>`，不再接受 `--port`、`--secret`、`--allowed-origin` 或端口环境变量覆盖。显式文件必须存在且可读、格式有效；加载、保存、配置锁、备份和引导恢复均使用选定路径。显式文件采用规范化绝对路径，其状态位于同目录 `.knot-state`，共享配置目录的进程还受目录实例锁保护。未传配置路径时使用 `~/.config/knot/config.toml` 和 `~/.local/state/knot`，不读取 XDG 数据目录环境变量。

core 不生成 `core.json` 或独立 token 文件，也不读取旧 token 作为认证来源。客户端自行保存 URL 和 Secret，通过版本及运行状态 API 校验连接。实例 ID 只在内存中维护，由 `/v1/runtime` 返回。启动配置 Secret 不通过业务配置 API 返回或修改，手动修改文件并重启后生效。

### 3. 认证

`service.secret` 非空时，所有业务 HTTP/WS API（含 version、runtime、health）统一认证；缺失返回 `401 AUTH_REQUIRED`，错误返回 `401 AUTH_FAILED`。支持 `Authorization: Bearer <Secret>`，保留 `X-Knot-Token` 兼容头。

Secret 缺省或为空时全部业务接口免 Secret 校验。两种模式均独立执行 Origin 校验。健康状态不会将免认证或没有运行文件视为故障。

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
