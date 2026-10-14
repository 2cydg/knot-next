# 连接资源

## 资源模型

连接是实际已认证 SSH transport，SSH/SFTP 会话和转发分别持有它的 lease。连接 ID 为 `conn_` 加 16 位随机十六进制；SSH 会话为 `ssh_...`，SFTP 为 `sftp_...`。调用者把它们作为 opaque string，不能截取、按前缀查找或拼接内部 pool key。持久 server/key/proxy/provider ID 不变；exec/transfer 编号保持现有契约。

```json
{
  "id": "conn_0123456789abcdef",
  "server_id": "srv_web",
  "alias": "web",
  "host": "example.invalid",
  "port": 22,
  "user": "deploy",
  "created_at": "2026-10-14T00:00:00Z",
  "sequence": 2,
  "target": true,
  "route": ["conn_fedcba9876543210"]
}
```

新 transport 分配新 ID 和建立序号；复用不会改变它们。`route` 是中间跳板连接 ID；`target=false` 表示目前只作为中间跳板，不能作为临时转发的默认目标。共享 transport 后来被直接使用时可以成为目标。响应不包含凭据摘要或内部 pool key。

## HTTP 接口

- `GET /v1/connections?server_id=...`：精确服务器过滤，返回数组，按建立序号升序排列；不传 server_id 返回所有当前连接。
- `GET /v1/connections/{id}`：返回实际连接，已关闭/不存在为 `404 NOT_FOUND`。连接查询不保留关闭历史，SSH/SFTP 的终态历史仍保留原 `connection_id`。
- `POST /v1/connections/clear`：既有全局主动断开操作，会影响所有共享 owner；普通关闭当前页面应 DELETE 自己的会话/转发。

`GET /v1/sessions?server_id=...&active=true` 返回该服务器所有 connected/attached/detached 来源候选，包括不同连接上的会话。精确匹配 server_id，不按显示 alias 或连接过滤；响应保留开启时间、当前目录、会话及连接 ID，前端负责分页和选择。

## 取得语义与边界

SFTP 显式传 `connection_id` 时使用该已认证身份。core 校验所属服务器、存活和完整路由，原子取得全部 lease；失效为 409，不回退新拨号。新保存的密码、代理或 host-key-policy 不会被假称应用到旧 transport。不传时仍按配置、凭据、Agent、host-key-policy 与完整路由身份匹配连接，不套用转发的“最新”选择。

连接、来源会话和 SFTP 传输连接独立：跨同服务器连接跟随不要求身份相同。关闭某个 SSH 会话不会关闭其他持有 lease 的 SFTP/转发。

生成器错误直接失败，注册锁内对 ID 冲突重试；最近关闭 ID 的保留窗口为 10 分钟/最多 4096 个，活跃 ID 不裁剪。ID 仅属于当前 daemon 实例，客户端重启/切换实例后重新发现并查询资源。

显式 connection_id 可以选择仅作为跳板的存活条目，只须服务器匹配且完整前缀仍存活；空 ID 的最新选择仍排除这些跳板。legacy GetClientContext 也记录完整路由并触发目标快照 hook，但调用方自身不持久拥有 lease；业务资源仍应使用 AcquireClientContext/AcquireConnection。
