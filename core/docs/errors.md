# Errors

HTTP 成功响应使用 `data`；失败响应使用统一 `error`，包含 `code`、`message`、`details`、`retryable`、`risk`、`resource` 和 `suggested_action`。客户端根据机器码和状态判断，不匹配英文错误文本。完整字段见 [API 约定](api/README.md)。

参数、未知 JSON 字段、超限或非法尾部返回 400；缺失资源返回 404；资源未就绪、状态冲突或订阅容量不足返回 409。认证缺失或错误返回 401；浏览器 Origin 不允许返回 403。SSH 创建返回 201 后仍可能 connecting/challenge/failed；客户端必须通过 GET/事件等待就绪。

exec 返回同步结果，远端非零退出保留 exit_code，并不等于框架失败。连接/执行失败保留 `framework_code`；无法在预算内完成收尾时提供 `cleanup_error`。取消和超时不意味着远端脱离 SSH channel 的进程已被杀死。见 [SSH/exec](api/sessions.md)。

传输有 completed、failed、partial_failed、canceled 四种终态；WRITE 后 CLOSE 失败必须返回失败并保留已复制字节，不能计作文件完成。GET Transfer 是事实来源，WS 断线后可恢复查询。见 [SFTP](api/sftp.md)。

认证成功后的保存失败为非致命 warning，不破坏已建立的连接。局部和全局事件保留脱敏 `warning.kind/message`；日志使用 warn。秘密、token、PTY 和文件内容不作为错误诊断输出。

## 本轮新增机器码

| code | HTTP / 含义 |
| --- | --- |
| HOST_KEY_CHALLENGE_EXPIRED | 409，展示的主机信任挑战已过期；迟到信任不写 known_hosts |
| AUTH_CHALLENGE_EXPIRED | 409，认证挑战已过期 |
| CHALLENGE_ABORTED | 资源/last_challenge.failure_code，用户拒绝或取消待应答挑战；迟到 POST 为 409 CONFLICT |
| CONNECTION_FAILED | 资源/last_challenge.failure_code，主机确认期间已观察到传输断开，不冒称过期 |
| UNSUPPORTED_PRIVATE_KEY | 400，当前不支持加密 PKCS#8 容器，路径/粘贴相同 |
| INVALID_PRIVATE_KEY | 400，损坏私钥或错误口令；PASSPHRASE_REQUIRED 继续表示缺口令 |
| FORWARD_STOP_INCOMPLETE | 409，停止已请求，实际协议收尾尚未完成；GET 原资源 |
| RESOURCE_LIMIT | 409，代理测试并发容量不足 |

proxy-tests 的 PROXY_* 失败是 HTTP 200 Result.failure_code（不是伪造配置失败）。forward listener/连接的 FORWARD_* 与 CONNECTION_CLOSED 通过 View.failure_code 查询；FORWARD_TARGET_FAILED 仅在单流失败事件中出现。配置模型校验可提供 details.fields 数组。重复/错轮挑战为 CONFLICT，终态历史裁剪后为 NOT_FOUND，不能误报挑战超时。

资源 failure_code 为 UPPER_SNAKE；SSH framework_code 保持 lower_snake（如 host_key_challenge_expired/auth_challenge_expired/connection_failed）。
