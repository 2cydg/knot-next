# Security

Reserved for the knot-core security model document.

文件日志使用有界 JSON lines 和统一脱敏（包含包装错误、slog group/With 属性、已注册请求/配置秘密、Authorization 与认证 URL）。秘密注册表有界，达到保护预算后自由文本诊断统一隐藏，避免淘汰旧秘密后泄漏。不记录 exec 正文、PTY 字节或文件内容。Unix 将日志目录/文件约束为 0700/0600；Windows 使用当前用户 SID 的 protected DACL，不把 Unix mode 位当作 Windows ACL 安全证据。Agent forwarding 为每个远端通道创建本地 Agent 连接，随共享 SSH client 关闭回收。
