# 转发配置与运行实例

## 持久定义

`GET /v1/config/servers/{id}/forwards` 返回定义数组。
`PUT /v1/config/servers/{id}/forwards` 整组替换，最多 128 条：

```json
{
  "forwards": [
    {"id": "rule_saved", "type": "local", "bind": "127.0.0.1:8080", "target": "127.0.0.1:80", "enabled": true},
    {"type": "dynamic", "bind": "[::1]:1080", "enabled": false}
  ]
}
```

新条目可省略 ID，由 core 随机分配；显式 ID 在服务器内唯一。`type` 为 local/remote/dynamic，bind 为明确 IP:port（IPv6 可带 zone）或 localhost:port，允许 0 请求随机端口。local/remote 的 target 为 host:1..65535，dynamic target 必须空。bind 可以为明确非 loopback 地址；这是转发 listener，与始终 loopback 的管理 API 分开。

定义只保存业务字段，不保存 runtime ID、连接或状态。配置保存不会热改已有 listener/流。每个实际目标连接第一次取得时固定初始定义快照并登记启用；复用、增加会话、并发取得和终态裁剪不重复自动创建。编辑只作用于以后新 transport。跳板中间条目不触发自动转发。自动失败保留已认证 SSH/SFTP，能取得运行 ID 时登记 failed 实例，全局事件包含 `auto_forward_failed` warning；运行 ID 分配失败的独立告警见后文。

手写 hostname bind（如 `myhost:8080`）不再通过保存/运行创建校验；已有配置只读仍原样返回，修正为显式 IP 或 localhost 后才能保存其他配置。target 的 hostname 不受影响。迁移步骤和边界见 [迁移兼容说明](../migration.md#私钥文件读取与-bind-校验的兼容性收窄)。

旧 TOML 的 `L/R/D + local_port/remote_addr` 映射到上述 DTO，默认 bind 为 127.0.0.1、enabled=true。无 ID 的旧规则用服务器及规则位置/内容的稳定非秘密映射，读取和普通 server 保存不生成随机 ID。首次 PUT 保存扩展字段前保留精确 `config.toml.pre-forwards.bak`。旧 loader 不理解 bind/target/enabled/id 扩展，不能准确执行任意绑定/禁用/随机端口；回退需恢复备份。普通 server PUT 保留原转发定义，不通过该 DTO 隐含改规则。

## 临时运行接口

- `GET /v1/forwards?server_id=...&connection_id=...`：查询数组，两个过滤均精确匹配。
- `POST /v1/forwards`：创建临时实例，返回 HTTP 202 和固定实际 connection_id。
- `GET /v1/forwards/{id}`：查询真实运行结果。
- `DELETE /v1/forwards/{id}`：停止并标为 deleted，保留有界历史。
- `POST /v1/forwards/{id}/control`：`{"op":"start"}` 或 `{"op":"stop"}`。
- `GET /v1/forwards/{id}/events`：WebSocket。

```json
{
  "server_id": "srv_web",
  "connection_id": "conn_0123456789abcdef",
  "type": "remote",
  "bind": "127.0.0.1:9000",
  "target": "127.0.0.1:3000"
}
```

省略 connection_id 时，按 server_id 选择建立序号最大的已认证**目标**连接并原子取得完整路由 lease。无连接/连接失效为 409，不自动连接。响应固定实际 ID，start/stop/delete 不再重新选最新连接。临时规则不写 TOML，core 重启不恢复。

## View、状态与流量

View 包含 `id`（forward_随机短 ID）、`server_id`、`connection_id`、`type`、原始 `bind/target`、实际 `bound_address`、`origin`（temporary/persistent）、可选 `definition_id`、`state`、`failure_code/error`、`created_at/updated_at/ended_at` 和 `events_url`。

| state | 语义/操作 |
| --- | --- |
| starting | 正在开 listener，GET/events 等待真实结果 |
| active | listener 已建立；单个流目标失败仅发布 flow_failed 事件，不给 active View 留下永久故障 |
| stopping | 已请求取消，仍等待实际网络/迟到协议工作 |
| stopped | 自己的流、listener、worker 和 lease 已释放；可在原连接上 start |
| failed | listener/连接失败；工作释放后可在仍存活的原连接上 start |
| deleted | 已停止，不能 start；GET 在历史裁剪前仍可查询 |

local 流通过 SSH direct-tcpip dial target；remote 使用真实 tcpip-forward/forwarded-tcpip，并在 core 本机拨 target；dynamic 是本地无认证 SOCKS5，仅 CONNECT，支持 IPv4/IPv6/domain，其他命令返回协议失败。单向 EOF 保留半关闭，取消关闭双向流。

运行条目（包括未完成收尾）最多 1024；最多 512 个并发流 owner、32 个实际 SSH dial worker，每实例最多 128 个已登记 socket（包含流两端）。超额流被关闭。`active_forwards` 仅统计 active。实例失败机器码包含 `FORWARD_START_FAILED`、`CONNECTION_CLOSED`、`AUTO_FORWARD_START_FAILED`。消息不回显任意远端错误或凭据。

stop/delete HTTP 等待最多 15 秒。尚未结束返回 `409 FORWARD_STOP_INCOMPLETE`，通过 GET 继续查询；不会提前归还 lease 或关闭他人的 pooled transport。x/crypto/ssh 的 pending Listen/Dial 不能本地撤回，迟到结果及 Close 仍被 owner 跟踪；真实 reply/transport 结束后才释放。shutdown 同时关闭 pool transport 并等待实际 worker，预算不足报告未完成。

终态在实际释放后按 ended_at 保留 10 分钟、最多 1024 条；查询/创建及收尾时裁剪。记录裁剪/core 重启后为 404，不伪称规则成功。订阅每实例最多 16 个，每个队列 32 条；满队列断开，GET 恢复。第一帧是 `{"type":"forward.snapshot","forward":{...}}`，后续事件为 `forward.starting/active/stopping/stopped/failed/deleted/flow_failed`，全局 events 同时发布必要通知。

`forward.flow_failed` 在事件顶层携带 `failure_code=FORWARD_TARGET_FAILED` 和脱敏 `error`，其中 forward View 保持 active 且无实例错误。全局事件也携带流失败码。重复 DELETE 不改变 ended_at，也不再次发布 deleted 事件。自动启动失败记录会淘汰最旧终态，容量不能静默吞掉规则失败；熵源失败至少发布失败警告。

SOCKS5 无方法时回 05 FF；无效版本直接关闭。拨号超时回 06，SSH 明确禁止回 02，其余无法判定的远端失败回 01，避免把未知原因伪称 host unreachable 或 connection refused。

当自动转发因熵源错误或碰撞重试耗尽而无法取得 ID 时，不构造空 ID 的 forward View，不生成 events_url，也不登记虚假的运行实例。服务告警事件为 `forward.auto_failed`，含 `auto_failure: {server_id, connection_id, definition_id}`、`failure_code=AUTO_FORWARD_START_FAILED` 和固定脱敏 error，省略 forward 字段。core 全局事件的 resource 为 connection，resource_id 为该已认证连接的真实 ID，data 保留规则 ID 及 `warning.kind=auto_forward_failed`。客户端按连接/定义显示失败提示，不对这一告警请求不存在的 forward GET/事件 URL。已有 failed 实例继续使用 `forward.failed` 和正常 View。
