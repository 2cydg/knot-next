# Config API

## 范围

本分册描述配置读取、校验、迁移和配置资源管理接口：

- config summary
- metadata
- validation
- migration
- settings
- servers
- proxies
- keys
- sync providers

## 资源模型

配置读接口统一返回“view”对象，不直接暴露 secret 明文。

### Summary

`GET /v1/config` 返回：

- `schema_version`
- `settings`
- `servers`
- `proxies`
- `keys`
- `sync_providers`
- `updated_at`
- `metadata`

其中：

- server 使用 `ServerProfileView`
- proxy 使用 `ProxyProfileView`
- key 使用 `KeyMetadataView`
- sync provider 使用 `SyncProviderView`

### SettingsView

磁盘 `default_sync_provider` 保存 alias，API 的该值继续为 ID。`recent_limit` 必须为非负整数；0 按旧配置规则归一为默认 5，不能用于关闭历史记录。

磁盘默认 alias 无法匹配时，运行时视为未设置，并在 summary 的 `metadata.warnings` 和 `/v1/config/metadata` 的 `warnings` 中报告 `settings/default_sync_provider`；读取不改写磁盘。API 设置默认项时仍校验 ID 必须存在。

每个普通设置字段都包装为：

```json
{
  "key": "log_level",
  "value": "info",
  "default": "info",
  "type": "string"
}
```

`sync_password` 不会返回明文，只返回：

```json
{
  "sync_password_set": true
}
```

### ServerProfileView

持久化的 `forwards` 当前不经配置 API 暴露，也不能经该 API 修改；普通 server 更新会保留原值，forwarding 引擎尚未接入。

```json
{
  "id": "srv_prod",
  "alias": "prod",
  "host": "10.0.0.1",
  "port": 22,
  "user": "root",
  "auth_method": "password",
  "password_set": true,
  "key_id": "key_main",
  "known_hosts_path": "/path/to/known_hosts",
  "proxy_id": "prx_1",
  "jump_host_ids": ["srv_jump"],
  "tags": ["prod"]
}
```

### ProxyProfileView

```json
{
  "id": "prx_1",
  "alias": "corp",
  "type": "socks5",
  "host": "127.0.0.1",
  "port": 1080,
  "username": "demo",
  "password_set": true
}
```

### KeyMetadataView

`private_key_set` 表示存在内联 private_key 或 SourcePath。新增 `fingerprint`（SHA256 公钥指纹）和 `encrypted`（SSH 私钥本身是否有口令）字段；后者与磁盘 `ENC:` 存储加密不同。普通创建/修改不能用任填 type/length 伪造 key 元数据；真实材料由 secret API 解析。

```json
{
  "id": "key_main",
  "alias": "main",
  "type": "ed25519",
  "length": 256,
  "private_key_set": true,
  "source_path": "/path/to/id_ed25519"
}
```

### SyncProviderView

```json
{
  "id": "sync_s3",
  "alias": "backup",
  "type": "s3",
  "bucket": "demo",
  "key": "prefix",
  "region": "ap-southeast-1",
  "endpoint": "https://s3.example.com",
  "access_key_id_set": true,
  "secret_access_key_set": true,
  "session_token_set": false,
  "path_style": true,
  "default": true
}
```

### ValidationResult

```json
{
  "valid": false,
  "errors": [
    {
      "resource": "servers.srv_prod",
      "field": "host",
      "message": "host is required"
    }
  ]
}
```

### Migration / MigrationPlan

业务配置使用兼容旧 Knot 的 TOML；HTTP 保持 JSON。默认原位置 TOML 直接复用，`needs_migration=false`。`source="legacy-toml"`，`json_exists` 报告保留的另一份 JSON。plan 的 `items` 描述 reuse / preserve / conflict，不返回秘密。

显式额外来源预览返回 `source_revision`、`target_revision` 和 `id_mappings`；详情见 [旧配置复用与导入](../migration.md)。

## HTTP 接口

接口清单：

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
- `GET /v1/config/servers/resolve`
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

### GET `/v1/config`

返回配置 summary。

### POST `/v1/config/validate`

校验整个配置对象。

请求体：完整 `config.Config` JSON。

说明：

- 这是纯校验接口，不写入配置
- 返回 `200`，结果在 `data.valid` / `data.errors`

### GET `/v1/config/metadata`

返回：

- `schema_version`
- `source`
- `config_path`
- `migration`
- `updated_at`

`warnings`（可选）包含读取兼容性警告，每条使用 `resource`、`field`、`message` 描述。

### GET `/v1/config/migration`

返回原位置复用 / 额外来源状态。默认 TOML 用户无需 apply。未知 TOML 字段允许读取，但会丢字段的修改被拒绝。

### GET `/v1/config/migration/plan`

无参数报告 active TOML 的复用和保留范围。额外 TOML 使用 `?source_path=/absolute/path/extra.toml&mode=fail_on_conflict`，返回脱敏计划、两端 hash revision 和 ID 映射；预览不写文件或初始化 crypto。JSON 来源明确拒绝。

### POST `/v1/config/migration/apply`

显式导入请求：

```json
{
  "source_path": "/absolute/path/extra.toml",
  "mode": "fail_on_conflict",
  "source_revision": "<preview source hash>",
  "target_revision": "<preview target hash>"
}
```

默认 `fail_on_conflict`；`skip_existing` 保留匹配目标，`overwrite` 更新匹配目标。alias 冲突复用目标 ID 并重映射引用；ID/alias 命中不同对象的歧义拒绝。revision 变化、重复旧 apply 返回 409。目标先备份为 `config.toml.import.bak` 再原子替换，来源和附属 trust/recent/material 不被改写。返回 config summary。

没有 `source_path` 的旧请求保留兼容入口：默认原位置复用只返回 summary，自定义目标使用发现的旧 TOML。需要绑定已审阅预览时必须显式传 source/revision。

### Settings

#### GET `/v1/config/settings`

返回 `SettingsView`。

#### GET `/v1/config/settings/{key}`

返回单个 setting。

#### PATCH `/v1/config/settings/{key}`

请求体：

```json
{
  "value": "debug"
}
```

返回更新后的单个 setting。

`value` 必须以目标 setting 的实际 JSON 类型传入，服务端不会根据 `type` 字段做字符串转型。

当前实现接受的类型规则：

- `forward_agent`、`clear_screen_on_connect`、`broadcast_escape_enable`: JSON boolean
- `broadcast_escape_char`、`idle_timeout`、`keepalive_interval`、`log_level`、`default_sftp_local_path`、`default_sync_provider`: JSON string
- `recent_limit`: JSON number

#### DELETE `/v1/config/settings/{key}`

将 setting 重置为默认值。

### Servers

#### GET `/v1/config/servers`

无 query 时返回完整 server 列表。

返回示例：

```json
[
  {
    "id": "srv_prod",
    "alias": "prod",
    "host": "10.0.0.1",
    "port": 22,
    "user": "root",
    "auth_method": "password",
    "password_set": true,
    "key_id": "key_main",
    "known_hosts_path": "/path/to/known_hosts",
    "proxy_id": "prx_1",
    "jump_host_ids": ["srv_jump"],
    "tags": ["prod"]
  }
]
```

当存在以下任一 query 时，返回分页结果 `Page[ServerProfileView]`：

- `page=true`
- `alias`
- `tag`
- `auth_method`
- `proxy_id`
- `q`
- `limit`
- `offset`
- `sort`

query 说明：

- `alias`: 精确 alias 过滤
- `tag`: 可重复，按 tag 过滤
- `auth_method`: 过滤认证方式
- `proxy_id`: 过滤代理
- `q`: 模糊搜索
- `limit`: 分页大小
- `offset`: 偏移
- `sort`: 排序字段

分页返回示例：

```json
{
  "items": [
    {
      "id": "srv_prod",
      "alias": "prod",
      "host": "10.0.0.1",
      "port": 22,
      "user": "root",
      "auth_method": "password",
      "password_set": true,
      "key_id": "key_main",
      "known_hosts_path": "/path/to/known_hosts",
      "proxy_id": "prx_1",
      "jump_host_ids": ["srv_jump"],
      "tags": ["prod"]
    }
  ],
  "total": 10,
  "limit": 5,
  "offset": 0
}
```

#### POST `/v1/config/servers`

创建 server。

请求体使用 `ServerProfile`，但不允许直接提交 `password` 明文。

#### GET `/v1/config/servers/{id}`

获取单个 server view。

#### PUT `/v1/config/servers/{id}`

更新 server。

说明：

- `id` 不能改
- 若请求体里直接包含 `password`，会被拒绝

#### DELETE `/v1/config/servers/{id}`

删除 server。

响应：

```json
{
  "deleted": true
}
```

#### GET `/v1/config/servers/resolve?ref={ref}`

按 `id` 或 alias 解析 server。

### Proxies

#### GET `/v1/config/proxies`

返回 proxy 列表。

#### POST `/v1/config/proxies`

创建 proxy。

说明：

- 不允许直接提交 `password`

#### GET `/v1/config/proxies/{id}`

#### PUT `/v1/config/proxies/{id}`

#### DELETE `/v1/config/proxies/{id}`

行为与 servers 相同。

### Keys

#### GET `/v1/config/keys`

#### POST `/v1/config/keys`

说明：

- 不允许直接提交 `private_key`

#### GET `/v1/config/keys/{id}`

#### PUT `/v1/config/keys/{id}`

#### DELETE `/v1/config/keys/{id}`

### Sync Providers

#### GET `/v1/config/sync-providers`

#### POST `/v1/config/sync-providers`

说明：

- 不允许直接提交 `password`
- 不允许直接提交 `access_key_id`
- 不允许直接提交 `secret_access_key`
- 不允许直接提交 `session_token`

#### GET `/v1/config/sync-providers/{id}`

#### PUT `/v1/config/sync-providers/{id}`

#### DELETE `/v1/config/sync-providers/{id}`

#### PUT `/v1/config/sync-providers/{id}/default`

将该 provider 设为默认同步 provider。

#### DELETE `/v1/config/sync-providers/{id}/default`

清除默认同步 provider。

说明：

- 当前路径里虽然包含 `{id}`，但删除默认项时不会校验该 `{id}` 是否存在
- 实际行为是全局清空 `settings.default_sync_provider`
- 只要清理动作成功，当前实现就返回 `200`；即使请求路径中的 `{id}` 不存在，也不会因此返回 `404`

## 常见错误

配置接口常见错误：

- `400 INVALID_JSON`
- `400 VALIDATION_FAILED`
- `404 NOT_FOUND`
- `409 CONFLICT`
- `500 INTERNAL_ERROR`

## 实现约束

- 配置写接口严格禁止在普通 config endpoint 中直接写入 secret 字段
- secret 必须经由 `secrets` 分册中的接口单独写入
- `GET /v1/config/servers` 在分页和非分页模式下返回结构不同，客户端需要按 query 是否触发分页自行处理
- 删除类接口的 `data` 结构并不全局统一；例如 config 和 sftp 常见为 `{ "deleted": true }`，连接清理接口则返回 `{ "closed": <n> }`

## 客户端建议

- 首次加载配置页时优先请求 `GET /v1/config`，而不是分别拉取多个子资源
- 需要增量编辑单个设置时使用 `/settings/{key}`，需要结构校验时使用 `/validate`
- 对 server、proxy、key、sync provider 的 secret 字段，始终走 `secrets.md` 中的接口

## 最近使用（2026-10-08）

服务器 view 新增 `last_used`（UTC RFC3339；未使用或超出历史保留上限时为 `null`）。`GET /v1/config`、服务器列表、单项 GET 和 resolve 返回同一历史视图。普通创建/更新的响应不代表一次成功连接，不应据此刷新最近使用；需要最新历史时重新 GET。

`GET /v1/config/servers?sort=recent` 返回分页对象，已使用目标按时间降序，时间相同按 alias、ID 排序，未使用目标排在最后。`sort=alias` 和默认排序按 alias、ID；继续支持 `host`、`created`/`id`。`started_at` 是会话创建时间，不能代替 `last_used`。

历史沿用 `<StateDir>/state.json` 的 `recent: [{server_id,last_used}]` 模型。SSH backend 就绪、SFTP subsystem open、exec 请求被远端接受时才记录**用户选择的目标**；跳板不记录，连接/认证/exec 请求拒绝不记录。exec 已开始后退出码非 0 或取消，仍算成功使用目标。SSH/SFTP 更新由所属 worker 原子保存，GET 可稍后观察到；正常退出等待这些 worker 收尾。

`recent_limit` 加载时 `<=0` 沿用默认 5；历史服务防御非法 limit，并将最大保存数量限制为 1024。按 ID 去重、并发读改写串行、失败写入不提交。删除服务器会裁剪历史，读取也过滤已删除 ID；历史是可选数据：读取发现非法 JSON 时，先把原件隔离为 `state.json.corrupt.1`–`.3` 中第一个空位，并记录 `recent.state.quarantined` 诊断，然后按空历史继续；查询不会制造一次成功使用，下一次成功回调保存新的 `state.json`；删除服务器的 prune 也可保存空的已过滤历史。隔离文件不自动删除或覆盖；三个空位用尽或隔离失败时，保留损坏原件并拒绝历史写入，配置摘要、列表、单项及 resolve 仍正常返回，`last_used=null`，诊断记录具体失败。保存或移走备份、恢复目录权限后可再次自动恢复。此降级只作用于历史；`config.toml` 解析/解密错误仍报错。历史写入失败不影响已成功的连接。

隔离先以 `O_CREATE|O_EXCL` 创建空占位文件，再 rename 原件；进程若在两步之间终止，可能留下空的 `.corrupt.N` 并占用一个备份位，损坏原件仍在 `state.json`。此类空位也不自动清理；确认并保存所需证据后可手动移走或删除以释放位置。

`default_sftp_local_path` 等客户端偏好保持旧字段兼容，core 不据此改变本地进程 cwd。独立客户端配置在 CLI 阶段承接。
