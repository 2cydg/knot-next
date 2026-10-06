# Secrets API

## 范围

本分册描述 secret 相关接口：

- secret 摘要
- 加密能力
- server password
- proxy password
- key private key
- sync password
- sync provider password
- sync provider s3 credentials

## 资源模型

### Secret Summary

`GET /v1/secrets` 返回：

```json
{
  "servers": {
    "srv_prod": true
  },
  "proxies": {
    "prx_1": false
  },
  "keys": {
    "key_main": true
  },
  "sync_providers": {
    "sync_s3": {
      "password": false,
      "access_key_id": true,
      "secret_access_key": true,
      "session_token": false
    }
  },
  "sync_password_set": true
}
```

说明：

- 返回的是“是否已设置”，不是明文
- `keys[id] == true` 表示存在 `private_key` 或 `source_path`

### Crypto Capability

`GET /v1/secrets/crypto` 返回：

```json
{
  "provider": "local-aes-gcm",
  "available": true,
  "limitations": [
    "local key file protects secrets; platform credential store integration is not active"
  ]
}
```

## HTTP 接口

接口清单：

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

### GET `/v1/secrets`

返回 secret 摘要。

### GET `/v1/secrets/crypto`

返回当前 secret 加密 provider 能力。

### Server Password

#### PUT `/v1/secrets/servers/{id}/password`

请求体：

```json
{
  "password": "super-secret"
}
```

返回更新后的 `ServerProfileView`。

#### DELETE `/v1/secrets/servers/{id}/password`

清除 server password，返回更新后的 `ServerProfileView`。

### Proxy Password

#### PUT `/v1/secrets/proxies/{id}/password`

请求体：

```json
{
  "password": "proxy-secret"
}
```

#### DELETE `/v1/secrets/proxies/{id}/password`

### Key Private Key

#### PUT `/v1/secrets/keys/{id}/private`

请求体：

```json
{
  "private_key": "-----BEGIN OPENSSH PRIVATE KEY-----...",
  "source_path": "/home/user/.ssh/id_ed25519"
}
```

返回更新后的 `KeyMetadataView`。

说明：

- `private_key` 和 `source_path` 都由这个接口管理
- 当只想更新 `source_path` 时，也可以传空 `private_key`

#### DELETE `/v1/secrets/keys/{id}/private`

清除 `private_key` 和 `source_path`。

### Sync Password

#### PUT `/v1/secrets/sync/password`

请求体：

```json
{
  "password": "sync-secret"
}
```

返回更新后的 `SettingsView`。

#### DELETE `/v1/secrets/sync/password`

### Sync Provider Password

#### PUT `/v1/secrets/sync-providers/{id}/password`

请求体：

```json
{
  "password": "provider-secret"
}
```

返回更新后的 `SyncProviderView`。

#### DELETE `/v1/secrets/sync-providers/{id}/password`

### Sync Provider S3 Credentials

#### PUT `/v1/secrets/sync-providers/{id}/s3-credentials`

请求体：

```json
{
  "access_key_id": "AKIA...",
  "secret_access_key": "secret",
  "session_token": ""
}
```

返回更新后的 `SyncProviderView`。

说明：

- `access_key_id` 与 `secret_access_key` 应成对提供
- `session_token` 可选

#### DELETE `/v1/secrets/sync-providers/{id}/s3-credentials`

清除 `access_key_id`、`secret_access_key`、`session_token`。

## 常见错误

- `400 INVALID_JSON`
- `400 VALIDATION_FAILED`
- `404 NOT_FOUND`
- `500 INTERNAL_ERROR`

## 实现约束

- 所有 secret 写入都依赖当前 crypto provider 可用
- provider 不可用时，底层会返回 `500 INTERNAL_ERROR`
- API 不提供任何读取 secret 明文的接口

## 客户端建议

- 修改 secret 前可以先读取 `/v1/secrets/crypto`，确认 provider 可用状态
- 读取配置展示时使用 `/v1/config` 或 `/v1/secrets` 的“是否已设置”信息，不要假设服务端支持 secret 回读
