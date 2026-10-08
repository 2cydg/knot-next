# Architecture

Core 是单用户、loopback-only 的能力服务。`cmd/core` 组织单实例锁、路径、token、logger、HTTP listener、runtime 发布与退出；`pkg/core` 组合能力并发布全局事件。配置和资源管理通过版本化 HTTP/JSON，SSH attach 与事件通过 WebSocket。

`pkg/config` 是配置唯一写入者，TOML 保存旧磁盘结构，JSON DTO 隐藏秘密。每次写操作在服务锁和文件锁内重新加载、验证并原子替换；认证成功后的 Remember 使用窄事务，只更新认证方式和选中的凭据。导入先做确定的身份映射和目标占用检查，冲突必须由计划暴露。

`pkg/sshpool` 持有共享 SSH client；session/SFTP 使用 lease 管理引用。连接、跳板 channel、交互 channel/PTY/env/shell 均有阶段期限。不能撤回的协议 worker 及迟到关闭保持跟踪与引用，服务退出在预算内等待并报告未完成清理；单个调用取消不关闭其他会话使用的 client。

`pkg/session` 拥有状态机、challenge、PTY pump、attach 和 exec；管理状态和原始终端字节分离。`pkg/sftp` 拥有 subsystem、路径操作、传输、目录 follow 和缓存。目录缓存最多 256 个目录和 4 MiB 估算保留成本，TTL 为 2 秒，失效代际阻止旧读取重新填入，关闭清空。目标文件关闭成功后才计入传输完成。

`internal/resourcepolicy` 分开限制活跃资源和终态历史，跟踪真正的 worker；订阅有容量限制。WS 写入在连接级串行并设置 5 秒期限，错误关闭连接并释放订阅；合法分片消息按完整消息交付，最大 1 MiB。

`pkg/recent` 独立维护旧 `state.json`，只记录成功使用的目标服务器。OSC7 只观察目录信息，PTY 字节原样通过。SFTP follow 显式绑定源 ID，访问验证和 generation 保护 CWD；失败手动 cd 保留当前状态。

`internal/logger` 管理有界文件轮转、脱敏与 Close。Agent Unix socket/Windows named pipe 共用拨号抽象，认证连接临时持有，转发 handler 随共享 client 释放。

CLI/TUI、转发资源管理、广播、归档、同步、更新、任务、自动 shell hook 和日志 tail API 在后续阶段。API 合同见 [API 入口](api/README.md)，平台验证边界见 [验收记录](baseline-acceptance.md)。
