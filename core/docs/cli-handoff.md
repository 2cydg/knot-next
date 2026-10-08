# CLI 接入交付清单

公开协议入口：[API](api/README.md)。可直接执行的真实调用链：[示例](../examples/baseline/README.md)。基线验证和平台边界：[验收](baseline-acceptance.md)。正式 CLI module 尚未开发。

1. 从本地 runtime 文件发现监听地址和 token_path，再携带 Bearer token 调用；runtime API 本身需要认证。离线时展示启动/重试，不用配置文件替代 API。
2. 201 只表示资源受理。SSH 等 connected，SFTP 等 open；host_key_pending/auth_pending 显示明确 challenge，响应后继续等待。GET 是事实来源；未知状态/机器码保留原值并显示可理解的失败，禁止猜测成功。
3. CRUD 使用无秘密 DTO；密码/私钥走 secrets API。passphrase 属于当前尝试；Remember 仅在认证成功后提交方式及所选凭据。订阅全局 warning 以观察异步保存失败，避免泄漏错误内秘密。
4. WS 初始 snapshot 与后续事件共同消费；断线重连后用 GET 恢复。传输 POST 202 后即使已完成也可从 snapshot/GET 得到 completed/failed/partial_failed/canceled。WS 事件只用于通知，不把断线当作 completed。
5. attach 的 binary 原样写终端，text 只解析控制事件；ANSI、CRLF、NUL、无效 UTF-8 不改写。保存/恢复终端模式，支持 resize，退出/取消/断网均恢复 raw mode。每流有序；跨 stdout/stderr 不承诺全局顺序。
6. detach 保留远端会话；disconnect 关闭当前资源。未就绪的 resize/signal/close_stdin 返回 409。慢 WS 会明确关闭，不能承诺收到最后事件；通过 GET 查询终态并显示截断。
7. exec 的 timeout_ms 包括连接阶段，0 允许长命令；客户端 HTTP timeout 应留出阶段和收尾预算。远端非零 exit_code 与 framework_code 分开展示；cleanup_error 表示实际后台清理尚未结束。
8. SFTP 当前目录由显式 cd 或 follow 控制；失败 cd 保留状态，成功 cd 暂停 follow。OSC7 需要远端已配置；客户端不自动注入 shell hook。最近使用只记录成功目标，按服务器 ID 管理候选。
9. 按 capabilities 和 health 展示 Agent/crypto/能力不可用，不能把未实现的转发/同步/归档等占位当作可用。log_level 修改需重启；log_path 支持文件诊断，当前没有日志 tail API。
10. Linux/macOS/Windows 原生终端、Agent、凭据库和 ACL 分别联测；交叉构建仅证明编译。基线不承诺未确认 SSH channel 在本地强制撤回；超时后保持引用和 worker 跟踪，shutdown 预算不足明确报错。

CI 使用 Go 1.27.1，全量三平台测试/构建、Linux race 和六目标 CGO=0 构建；Windows 输出 `.exe`。格式检查与 vet 使用固定 Go 工具链，日志和构建产物保存为 workflow artifacts。CI 定义不等于已运行的远端 CI 结果。
