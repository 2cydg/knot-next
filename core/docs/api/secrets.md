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
- 密码、S3 凭据字段写入空字符串表示清除，对应 presence 为 false；S3 的 access/secret 两字段仍须成对设置或清除。
- `keys[id] == true` 表示存在 `private_key` 或 `source_path`

### Crypto Capability

`GET /v1/secrets/crypto` 返回：

```json
{
  "provider": "linux-secret-service",
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
  "passphrase": "optional, validation only"
}
```

返回更新后的 `KeyMetadataView`。

说明：

- 选择 `private_key` 或 `source_path`，不能同时传非空值。SourcePath 由 core 在本机读取。
- 私钥实际解析为 Ed25519 / RSA / ECDSA 并计算类型、位数、指纹；损坏 PEM 或公钥不能当作私钥保存。
- 内联有口令私钥必须传正确 passphrase 以验证整个私钥。缺失返回 `400 PASSPHRASE_REQUIRED`，错误返回 `400 VALIDATION_FAILED`；passphrase 不落盘。原始有口令 key 仍加密保存，连接时需要本次尝试的 passphrase。
- SourcePath 可无口令登记有口令 OpenSSH 或传统加密 PEM key。OpenSSH 从 public envelope 派生元数据；PEM 无 public envelope，未解锁时 type/length/fingerprint 留空，传正确的验证口令可补齐。未解锁的 PEM 仅验证格式和加密头，私钥内容在提供口令时验证。路径丢失、不可读、内容变化不能复用旧 signer/cache 假成功。
- 无口令私钥忽略多余的 passphrase；有口令私钥仍严格验证口令。
- `encrypted` 表示 SSH key 自身需要口令，所有写入的内联 key 在 TOML 中还使用兼容旧版的 `ENC:`。
- 删除清空 key 材料及派生元数据。

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

- 非空 secret 写入依赖当前 crypto provider 可用；空值不加密。保存仍校验配置中其他已有密文可解密。
- 所需 provider 不可用时，底层会返回 `500 INTERNAL_ERROR`
- API 不提供任何读取 secret 明文的接口

## 客户端建议

- 修改 secret 前可以先读取 `/v1/secrets/crypto`，确认 provider 可用状态
- 读取配置展示时使用 `/v1/config` 或 `/v1/secrets` 的“是否已设置”信息，不要假设服务端支持 secret 回读
