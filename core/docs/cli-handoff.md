# CLI 接入交付清单

公开协议入口：[API](api/README.md)。可直接执行的真实调用链：[示例](../examples/baseline/README.md)。基线验证和平台边界：[验收](baseline-acceptance.md)。正式 CLI module 尚未开发。新增 core 能力的执行证据见 [本轮验收](gap-acceptance.md)，受控公开调用示例见 [gaps](../examples/gaps/README.md)。

1. 客户端显式配置 URL/Secret，只通过公开 HTTP/WS API 通信。core 不发布 runtime/token 文件；Secret 非空时包括版本接口在内均需要 Bearer 认证，空时免 Secret 校验，Origin 检查保持独立。自动启动只传 `--config <path>`，客户端不读取 core 配置。首次配置完成后恢复管理、页面直达或别名直连入口。
2. 201 只表示资源受理。SSH 等 connected，SFTP 等 open；host_key_pending/auth_pending 显示明确 challenge，响应后继续等待。GET 是事实来源；未知状态/机器码保留原值并显示可理解的失败，禁止猜测成功。
3. CRUD 使用无秘密 DTO；密码/私钥走 secrets API。passphrase 属于当前尝试；Remember 仅在认证成功后提交方式及所选凭据。订阅全局 warning 以观察异步保存失败，避免泄漏错误内秘密。
4. WS 初始 snapshot 与后续事件共同消费；断线重连后用 GET 恢复。传输 POST 202 后即使已完成也可从 snapshot/GET 得到 completed/failed/partial_failed/canceled。WS 事件只用于通知，不把断线当作 completed。
5. attach 的 binary 原样写终端，text 只解析控制事件；ANSI、CRLF、NUL、无效 UTF-8 不改写。保存/恢复终端模式，支持 resize，退出/取消/断网均恢复 raw mode。每流有序；跨 stdout/stderr 不承诺全局顺序。
6. detach 保留远端会话；disconnect 关闭当前资源。未就绪的 resize/signal/close_stdin 返回 409。慢 WS 会明确关闭，不能承诺收到最后事件；通过 GET 查询终态并显示截断。
7. exec 的 timeout_ms 包括连接阶段，0 允许长命令；客户端 HTTP timeout 应留出阶段和收尾预算。远端非零 exit_code 与 framework_code 分开展示；cleanup_error 表示实际后台清理尚未结束。
8. SFTP 当前目录由显式 cd 或 follow 控制；失败 cd 保留状态，成功 cd 暂停 follow。OSC7 需要远端已配置；客户端不自动注入 shell hook。最近使用只记录成功目标，按服务器 ID 管理候选。
9. 按 capabilities 和 health 展示 Agent/crypto/能力不可用，forward/proxy_test 已实现；同步/归档等仍 planned。log_level 修改需重启；log_path 支持文件诊断，当前没有日志 tail API。
10. Linux/macOS/Windows 原生终端、Agent、凭据库和 ACL 分别联测；交叉构建仅证明编译。基线不承诺未确认 SSH channel 在本地强制撤回；超时后保持引用和 worker 跟踪，shutdown 预算不足明确报错。

CI 使用 Go 1.27.1，全量三平台测试/构建、Linux race 和六目标 CGO=0 构建；Windows 输出 `.exe`。格式检查与 vet 使用固定 Go 工具链，日志和构建产物保存为 workflow artifacts。CI 定义不等于已运行的远端 CI 结果。

本轮前端契约：

- 所有连接/SSH/SFTP ID 为 opaque string；候选用 sessions?server_id=...&active=true，配对真实 connection_id。初次选源若希望复用其身份，显式填 SFTP connection_id；set-follow/clear-follow 后 transport 不变，目录应用异步，用 GET/events 判断结果。active 且 follow_source_dir 空时继续等待 OSC7。
- 每次挑战保存 challenge_id，POST 精确绑定该轮。后台 60 秒过期后前端可保留原框，Enter 按 HOST_KEY_CHALLENGE_EXPIRED/AUTH_CHALLENGE_EXPIRED 报错；404 表示记录不可查询。勿重发连接请求试图让旧挑战复活。
- 路径导入缺口令后询问重试；source_path/private_key 二选一，原文永久加密保存，口令只用于本次验证。普通资料与 secret 两步写入，部分成功只重试失败步骤。
- UI 选中文件使用 batch-download source_mode=literal；stat=false/目录列表不等于业务 cd。明确 symlink 与 directory，rename overwrite=false 仅能预检已有目标，不声称消除远端竞争。
- 代理测试使用当前草稿/保存 ID，明确 password 省略与空值，展示 endpoint/negotiation/tunnel 实际范围。无 target 的 HTTP TCP 通不代表 CONNECT/认证成功。
- 临时转发省略 connection_id 才使用最新目标连接；POST 202 后查询 starting/active/failed，停止固定原连接。服务器持久规则仅影响新 transport，自动失败只警告。WS 断流用原 ID GET，停止超时显示实际收尾待完成。
- 解码完整 GET/control/snapshot 时创建新的 DTO，再替换模型；omitempty 的清空关联字段在 JSON 中可能省略，不能把响应合并进仍带旧值的结构体。


## CLI/Core 连接改造验收（2026-10-30）

实现范围：core 从 `[service]` 加载端口、Secret 和 Origin；显式配置文件路径贯穿保存、加密初始化、锁及引导恢复。实例身份保存在内存，HTTP/WS 使用统一的可选 Secret 认证。客户端使用独立偏好文件中的 URL/Secret，提供首次配置表单及核心设置入口，并按需启动后台进程。

`knot-core init` 在当前目录创建 `config.toml`，填入默认端口 17898 和随机 32 位数字、大小写字母混合 Secret，不启动服务、不覆盖已有文件。启动使用 `knot-core --config <文件>`；免 Secret 模式需在文件中手动设为空字符串或省略 Secret。客户端不读取或修改 core 配置内容。

验证结果：

- `KNOT_TEST_CORE_BIN=<本次构建的 knot-core> scripts/test.sh --race`：两个 module 完整测试通过，包含真实 core API、进程启动、引导恢复、SSH/WS、事件恢复、Linux 伪终端及首次配置后别名直连交接。
- `scripts/lint.sh`：gofmt 和两个 module 的 go vet 通过。
- `scripts/build.sh`：原生 Linux 二进制已更新至 `knot-next/bin/knot-core`、`knot-next/bin/knot`。
- `scripts/build.sh --all`：Linux、macOS、Windows 的 amd64/arm64 共六组交叉编译通过。
- 独立 Linux CLI/core 进程实测：在隔离临时目录中先运行 init，再经伪终端填写 URL、Secret、自动启动选项和含空格的程序路径；配置路径自动补全，服务启动并保存连接。CLI 经 Ctrl+c 确认退出返回 130 后，core 仍可通过版本 API 访问。Secret 未出现在终端输出，core 配置字节未被 CLI 修改，未生成 core.json 或 token 文件。实测服务最后通过 API 关闭并确认释放端口。

测试还覆盖：语言与 core 设置互相保留、旧语言文件触发首次设置、空 Secret 有效、80×24/120×35 双语表单、动态字段、路径手填与自动来源、重复提交/过期结果/取消后不保存、认证和协议错误不启动、超时不启动、并发复用启动、子进程提前退出时仍等待竞争实例、已启动服务断线后不循环拉起、配置目录跨状态目录的实例锁，以及旧运行文件保持原样。

macOS/Windows 原生后台进程、终端及凭据存储行为仍待实机验收；交叉编译结果只证明编译通过。
