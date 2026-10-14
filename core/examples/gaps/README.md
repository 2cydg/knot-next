# Core 缺口契约公开 API 演示

在 core 目录执行以下受控演示。它们通过 HTTP/WS 调用真实本机 SSH/SFTP/SOCKS5/HTTP 代理/echo fixture，使用临时目录、随机端口和人工凭据，不访问用户配置或外部 SSH。

```bash
GOWORK=off GOCACHE=/tmp/knot-core-gap-build go test -v -count=1 -timeout=3m ./tests/integration -run 'Test(CrossConnectionFollow|FollowClearRejects|FollowSubscriptionCapacity|HostKeyExpiry|LiteralSymlinkRename|ProxyDraft|ForwardLocalRemoteDynamic|AutoForwardsSnapshot|IPv6Forwards|EncryptedPrivateKeyChallenges)'
```

| 场景 | 可审阅调用链 |
| --- | --- |
| 短 ID / 来源选择 / 换源 | follow_gap_test.go：同服务器不同凭据的连接，配对校验、set/clear、来源退出，SFTP ID/transport 保持 |
| 后台信任过期 | challenge_gap_test.go：真实默认 60 秒期限，GET 终态，迟到 POST 的过期码，known_hosts 字节不变 |
| 路径口令重试 | config_compatibility_test.go：source_path/passphrase、认证缺/错/对口令；已有 legacy SourcePath 与导入后原文行为分别覆盖 |
| 精确文件和链接 | file_gap_test.go：HTTP upload → literal batch-download，Lstat/Stat、删除链接、rename no-replace 冲突 |
| 代理测试 | proxy_gap_test.go：草稿/已存密码、省略/空值、实际 scope、协商/认证/目标拒绝和期限，无配置写入 |
| 临时 / 自动转发 | forward_gap_test.go：真实 L/R/D 双向 bytes、关闭 SSH 后继续、固定连接 stop/start/delete，自动快照/去重/冲突、最新连接、IPv6 |

私钥 CRLF 原文与并发别名保持、代理 caller cancel/shutdown、迟到 remote forward request 的 lease 所有权另有定向领域回归。面向实际 daemon 的简单工作流仍见 [baseline 示例](../baseline/README.md)，完整请求 DTO 与边界见 [API](../../docs/api/README.md)。

这些演示不替代真实终端或非 Linux 原生平台验收，也不提供正式 CLI/TUI。
