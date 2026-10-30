# Knot

现代化的 SSH 连接管理器，通过本地 HTTP/WebSocket API 提供强大的自动化能力。

## 概览

本仓库包含 **knot-core**：一个本地能力服务，通过标准化的 HTTP/WebSocket API 提供 SSH/SFTP 功能。

knot-core 可被以下客户端使用：
- 终端客户端（双语资源管理已实现，SSH/SFTP 终端流程开发中）
- 命令行脚本
- AI 代理和自动化工具
- 第三方应用程序
- 基于浏览器的界面

## 架构

```
脚本 / 终端 / AI / 第三方客户端
            │
      HTTP/JSON + WS
            │
        knot-core
            │
   配置/secrets + SSH pool
   + session/SFTP/forward/...
            │
       远端 SSH 主机
```

## 系统要求

- Go 1.27.1 或更高版本
- Linux、macOS 或 Windows
- 支持的架构：amd64、arm64

## 从源码构建

```bash
# 构建 knot-core
cd core
go build -o ../bin/knot-core ./cmd/core

# 或使用构建脚本
./scripts/build.sh
```

## 安装

1. 启动 core 服务：
   ```bash
   knot-core init
   knot-core --config ./config.toml
   ```

2. 使用 HTTP/WebSocket API 管理连接：
   ```bash
   # 检查服务状态
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/status
   
   # 列出服务器
   curl -H "Authorization: Bearer <token>" http://localhost:17898/v1/config/servers
   ```

完整 API 参考请见 [API 文档](core/docs/api/README.md)。

## 文档

- [API 参考](core/docs/api/README.md) - HTTP/WebSocket API 文档
- [架构设计](core/docs/architecture.md) - 系统架构和设计
- [安全模型](core/docs/security.md) - 安全模型和最佳实践
- [迁移指南](core/docs/migration.md) - 从旧版 knot 迁移

## 项目状态

本项目正在开发中，正在从原始 knot 代码库重构。

当前能力：
- ✅ 服务器配置管理
- ✅ SSH 连接池与保活
- ✅ SFTP 文件操作
- ✅ 密钥和代理管理
- ✅ 密钥加密（平台相关）
- ✅ 本地/远程/动态转发、持久定义及临时运行规则
- 🚧 广播模式（计划中）
- 🚧 配置同步（计划中）
- 🚧 TUI 界面（双语资源 CRUD、转发管理和原生 SSH 已实现，SFTP 文件页开发中）

独立 [CLI module](cli/README.md) 支持本地帮助/版本，以及真实 core URL/Secret 连接、
双语资源 CRUD、搜索、详情、代理测试、同步凭据、转发管理、离线恢复及带信任/认证交互的原生 SSH。
首次配置支持按需启动 knot-core；SFTP 文件页与传输仍在开发中。TUI 中按 c/Enter 登录，结束后返回服务器页；
`knot <alias>` / `knot ssh <alias>` 直连并保留远端退出码。根构建、测试、lint 脚本依次处理
core 和 cli。交付范围与验证见工作区根目录的 [阶段记录](../docs/cli-tui-progress.md)。

## 许可证

MIT 许可证 - 详见 [LICENSE](LICENSE) 文件。

## 贡献

开发指南和架构细节请参见 [AGENTS.md](AGENTS.md)。
