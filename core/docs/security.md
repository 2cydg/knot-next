# Security

服务只监听 IPv4/IPv6 loopback，HTTP/WS 必须携带 token；Origin allowlist 是浏览器附加限制。token 由 daemon 创建，位于 state/runtime；本地 discovery 读取 runtime，再读取其 token_path。token 不进入业务配置或配置 API。

秘密 API 默认只返回 presence/capability，密码和私钥采用旧 Knot 兼容的加密与平台 provider；测试使用注入的静态 provider，不能依赖真实凭据库。private-key passphrase 仅用于当前尝试，Remember 只在成功后保存认证选择。普通脱敏 CRUD 不接受秘密或清除隐藏的秘密。

HTTP JSON 严格限制 1 MiB，WS 消息 1 MiB，控制帧执行 masking/FIN/opcode/长度/UTF-8/关闭码校验。文件路径通过对应远端或本地 sandbox resolver；PTy 二进制数据不解释或改写。缓存、输出、历史、订阅和日志均有资源上限。取消只关闭本次 channel，避免破坏共享 SSH 连接。

平台行为必须原生验证。Linux 受控协议已验证；macOS/Windows 凭据库、Agent、ACL 和终端运行测试按已确定范围留到 CLI，交叉编译不能替代它们。

文件日志使用有界 JSON lines 和统一脱敏（包含包装错误、slog group/With 属性、已注册请求/配置秘密、Authorization 与认证 URL）。秘密注册表有界，达到保护预算后自由文本诊断统一隐藏，避免淘汰旧秘密后泄漏。不记录 exec 正文、PTY 字节或文件内容。Unix 将日志目录/文件约束为 0700/0600；Windows 使用当前用户 SID 的 protected DACL，不把 Unix mode 位当作 Windows ACL 安全证据。Agent forwarding 为每个远端通道创建本地 Agent 连接，随共享 SSH client 关闭回收。
