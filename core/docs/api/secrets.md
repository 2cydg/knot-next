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
- 支持 OpenSSH、未加密 PKCS#8 以及传统 RSA/EC PEM 容器，私钥实际解析为 Ed25519 / RSA / ECDSA 并计算类型、位数、指纹；损坏 PEM 或公钥不能当作私钥保存。
- 当前不支持 ENCRYPTED PRIVATE KEY（加密 PKCS#8）；路径/粘贴均返回 400 UNSUPPORTED_PRIVATE_KEY，与损坏私钥和缺口令区分。导入前请使用支持的容器格式。
- 支持的内联有口令私钥必须传正确 passphrase 以验证整个私钥。缺失返回 `400 PASSPHRASE_REQUIRED`，错误返回 `400 INVALID_PRIVATE_KEY`；passphrase 不落盘。原始有口令 key 仍加密保存，连接时需要本次尝试的 passphrase。
- source_path 是本次导入输入，必须为 core 所在系统的绝对路径（Windows 支持盘符/UNC），是可读普通文件。core 一次读取、完整验证后把**原始 bytes**加密保存为 PrivateKey，清空 SourcePath；不去口令重编码，不写源文件，不存口令。路径与粘贴等价，缺口令同样 PASSPHRASE_REQUIRED，错误为 INVALID_PRIVATE_KEY。材料上限 1 MiB（内联还受 JSON 整体 1 MiB 限制）。源文件后续删除/替换不影响已存材料。
- 普通 CreateKey/UpdateKey 不允许建立新的 SourcePath/材料引用。已有磁盘 SourcePath-only 记录兼容读取，GET 不自动迁移；显式重新导入成功后才取消引用。导入失败保持旧材料，并发普通别名编辑保留；并发材料更新为 409。
- 无口令私钥忽略多余的 passphrase；有口令私钥仍严格验证口令。
- `encrypted` 表示 SSH key 自身需要口令，所有本次导入的 key 在 TOML 中还使用兼容旧版的 `ENC:`。
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

显式 TOML 导入也按绝对路径、普通文件、1 MiB 上限读取 source_path，将原始 bytes 转为内联密文并清空路径；普通磁盘加载保留旧路径兼容语义。
