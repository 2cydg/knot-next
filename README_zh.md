# Knot

现代化的 SSH 连接管理器，通过本地 HTTP/WebSocket API 提供强大的自动化能力。

## 概览

本仓库包含终端客户端 **knot** 和本地服务 **knot-core**。Core 通过 HTTP/WebSocket API 提供 SSH/SFTP 功能。

knot-core 可被以下客户端使用：
- 终端客户端（支持双语资源管理和原生 SSH，SFTP 文件界面开发中）
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

- Linux、macOS 或 Windows，架构为 amd64 或 arm64
- 从源码构建需要 Go 1.27.1 或更高版本

## 从源码构建

以下命令在 `knot-org` 工作区根目录执行。

```bash
./scripts/build.sh
./scripts/test.sh
./scripts/lint.sh
# 交叉编译六组系统和架构组合
./scripts/build.sh --all
```

构建产物写入 `knot-next/bin/`。如果单独检出了 `knot-next`，可以分别在 `core/` 和 `cli/` 中用 `GOWORK=off` 执行 Go 命令，构建入口依次是 `./cmd/core` 和 `.`。[GitHub Actions](.github/workflows/ci.yml) 直接在该检出目录运行 Go 检查和构建。

## 安装

从 [v0.1.0 发布页](https://github.com/2cydg/knot-next/releases/tag/v0.1.0) 下载并解压源码，或运行 `git clone --depth 1 --branch v0.1.0 https://github.com/2cydg/knot-next.git` 获取安装脚本。进入源码目录后执行以下命令。

Linux 和 macOS

```sh
sh scripts/install.sh --version v0.1.0
```

Windows PowerShell

```powershell
.\scripts\install.ps1 -Version v0.1.0
```

省略版本参数可安装最新正式版。安装器验证 SHA-256 校验和，默认将 `knot` 和 `knot-core` 安装到 `~/.local/bin`。Windows 下两个程序都带 `.exe` 后缀。安装器按需初始化本地连接配置。运行 `knot` 可打开终端界面，启用自动启动时会按需启动 Core。

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
首次配置支持按需启动 knot-core。Core API 支持 SFTP 操作和传输，SFTP 文件界面仍在开发中。TUI 中按 c/Enter 登录，结束后返回服务器页。
`knot <alias>` / `knot ssh <alias>` 直连并保留远端退出码。根构建、测试、lint 脚本依次处理
core 和 cli。交付范围与验证见工作区根目录的 [阶段记录](../docs/cli-tui-progress.md)。

## 许可证

MIT 许可证 - 详见 [LICENSE](LICENSE) 文件。

## 贡献

开发指南和架构细节请参见 [AGENTS.md](AGENTS.md)。
