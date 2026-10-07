# Architecture

Reserved for the knot-core architecture document.

`pkg/recent` 独立维护旧 `state.json`，与加密业务配置分离；config service 提供 optional `RecordUse(serverID)` 成功回调，SSH/SFTP owner worker 和 exec 接受节点调用，只记录目标。`internal/logger` 提供实例 slog/File、脱敏注册表、串行轮转与明确 Close；入口在单实例锁取得后打开，生命周期末尾关闭。`pkg/sshpool` 统一 Unix socket/Windows named pipe Agent 拨号，认证连接临时持有，转发 handler 随共享 client 持有；SFTP follow 显式绑定来源 ID，以实际目录访问校验提交 CWD，并用 generation 阻止旧结果覆盖手动 cd/暂停/关闭。
