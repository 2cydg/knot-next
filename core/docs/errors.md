# Errors

HTTP 成功响应使用 `data`；失败响应使用统一 `error`，包含 `code`、`message`、`details`、`retryable`、`risk`、`resource` 和 `suggested_action`。客户端根据机器码和状态判断，不匹配英文错误文本。完整字段见 [API 约定](api/README.md)。

参数、未知 JSON 字段、超限或非法尾部返回 400；缺失资源返回 404；资源未就绪、状态冲突或订阅容量不足返回 409。认证缺失或错误返回 401；浏览器 Origin 不允许返回 403。SSH 创建返回 201 后仍可能 connecting/challenge/failed；客户端必须通过 GET/事件等待就绪。

exec 返回同步结果，远端非零退出保留 exit_code，并不等于框架失败。连接/执行失败保留 `framework_code`；无法在预算内完成收尾时提供 `cleanup_error`。取消和超时不意味着远端脱离 SSH channel 的进程已被杀死。见 [SSH/exec](api/sessions.md)。

传输有 completed、failed、partial_failed、canceled 四种终态；WRITE 后 CLOSE 失败必须返回失败并保留已复制字节，不能计作文件完成。GET Transfer 是事实来源，WS 断线后可恢复查询。见 [SFTP](api/sftp.md)。

认证成功后的保存失败为非致命 warning，不破坏已建立的连接。局部和全局事件保留脱敏 `warning.kind/message`；日志使用 warn。秘密、token、PTY 和文件内容不作为错误诊断输出。
