# 旧实现兼容 fixture

来源：旧 `knot` 提交 `e0b4d51eea6647e192371059381039b99fb301a2`。

`generate.go` 直接调用旧 `pkg/config.Config.SaveToPath`、`pkg/crypto.EncryptWithKey`、`DeriveKey` 和 `NewState`，于 2026-10-07 生成一次。测试读取提交的固定文件，不使用新加密器动态生成预期密文。

- `linux-secret-service`：固定 raw AES key，第 i 字节为 `255-i`，旧身份 `knot` / `knot-master-key`。
- `linux-machine-id`：salt 第 i 字节为 i；测试身份 `fixture-machine`、UID 1000；旧 PBKDF2-SHA256（100000 次）、材料 `fixture-machine\x001000`。
- 私钥由 `SHA256("Knot legacy fixture artificial private key")` 作为 Ed25519 seed，采用旧调用路径保存；它仅为人工测试 key。
- 所有密码和同步凭据都是 `fixture-*` 人工秘密，预期明文 SHA256 在 `plaintext-hashes.json`。`plaintext.toml` 是旧加载器回读并编码的人工明文样本。
- `.crypto-state` 使用旧的单层 Base64 探针；创建/更新时间固定。`known_hosts` 含 hashed host + 非默认端口；`state.json` 包含合法和已删除 server ID；TOML 覆盖 forwards、客户端偏好、默认同步 alias、WebDAV/S3 凭据与跨资源引用。
- `config.toml.lock` 是旧 SaveToPath 创建的空锁文件，也是复制旧安装时可能已有的文件。
- `readback.go` 从旧 module 调用旧加载器检查新 core 写回。新扩展字段由旧加载器忽略；旧版并不支持 SourcePath-only key。

在工作区根目录下有旧 `knot/` 时，生成命令（重新生成会有新的随机 nonce / probe，请有意更新 fixture）：

```bash
cd knot
GOWORK=off go run ../knot-next/core/pkg/config/testdata/legacy/generate.go \
  ../knot-next/core/pkg/config/testdata/legacy
```

独立旧版回读验证：

```bash
cd knot-next/core
KNOT_COMPAT_WRITEBACK_PATH=/tmp/knot-old-readback.toml GOWORK=off \
  go test -count=1 -run '^TestLegacyFixtureDirectReadAndWriteback$' ./pkg/config
cd ../../knot
GOWORK=off go run ../knot-next/core/pkg/config/testdata/legacy/readback.go /tmp/knot-old-readback.toml
```

真实平台 Secret Service / Keychain / DPAPI 的原生凭据库测试没有借用这些人工 fixture 宣称已完成；DPAPI 不能跨机器/账号固定解密。
