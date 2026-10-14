# Core 缺口补齐交付与验收

日期：2026-10-14。目标：knot-next/core，原基线 709b462，Go 1.27.1。本记录对应 docs/running/core缺口补齐开发计划.md；既有验收记录保留原日期的历史结果，本记录说明本轮新增行为。源码、测试、公开契约及示例已更新，未提交或发布。

## 交付范围

| 计划 | 本轮实现与证据 |
| --- | --- |
| C00 | 统一严格 JSON/secret 分离/GET 兜底，新增 connections、proxy-tests、forwards 分册；受控 SSH 增加真实远程转发与 reply barrier，本机代理/echo fixture |
| C01 | conn/ssh/sftp 随机短 ID、注册锁内碰撞重试；实际 transport 的 UTC 建立时间/序号、目标与跳板关系；精确/最新完整路由 lease；来源查询 server_id/active；近期关闭 ID 有界保留 |
| C02 | challenge_id/expires_at/outcome/failure_code、终态 last_challenge；同锁提交接受/过期；迟到 POST 返回专用过期码，GET/事件一致；真实默认 60 秒过期、known_hosts 字节保持 |
| C03 | 同服务器不同凭据、不同连接的来源配对；set/clear、订阅失败回滚、满容量同源幂等；独立传输/来源 ID；取消旧校验 context、generation 防迟到结果；每目标最多 8 个 follow worker |
| C04 | 文件/粘贴统一完整验证，绝对可读普通文件、1 MiB 限额、原始 bytes 加密，清 SourcePath，口令不落盘；CRLF、Ed25519/RSA/ECDSA、失败旧材料保持、并发别名保持；旧磁盘路径记录仍兼容读取 |
| C05 | 草稿/已存 ID、密码省略与显式空值、协议切换隔离、真实 SOCKS5/HTTP CONNECT 与准确 scope；正式 SSH 与测试共用协商；期限、caller cancel、shutdown、独立容量且不写配置 |
| C06 | 服务器转发整组 GET/PUT，定义/runtime 分离；L/R/D 旧格式映射与稳定 ID，保留普通 server 保存/导入，扩展写入前保留原 TOML 备份 |
| C07 | 真实 local/remote/dynamic、临时实例、每目标连接初始自动规则快照/并发去重；最新连接原子选择；流/dial/历史/订阅有界；listener/双向流/迟到请求/完整 lease 随实例持有；状态、局部/全局事件、active_forwards 与 shutdown 接入 |
| C08 | literal 下载、symlink 类型/Lstat/删除链接本身、严格稳定降序、rename overwrite 冲突与实际原子性边界；provider 换类型清旧凭据的事务；主要配置模型校验字段详情与旧文档差异修正 |
| C09 | API、错误、streaming、架构、迁移、CLI 交接、公开示例与仓库状态同步；全量/并发检查、六目标编译与平台边界明确 |

现有 PTY bytes/exec 真实退出、成功认证后 Remember、共享 lease、异步创建、资源保留、原位置 TOML/ENC/provider、recent/CWD/Agent/日志脱敏回归继续通过。未开发正式 CLI/TUI、设置偏好 UI/迁移、广播、归档、同步执行、更新或通用任务。

## 定稿的默认语义

- SFTP 明确分开 connection_id 和 follow_connection_id。首次希望使用来源身份时，调用者同时显式填写传输连接 ID；省略仍按现有配置身份匹配，不暗选“最新”。换源/clear 保持 transport。
- 持久转发编辑只影响以后新 transport；旧实例保留快照。自动失败不破坏已认证 SSH/SFTP，通过 failed 实例及 warning 查询。当前连接即使初始无规则，也只登记一次快照，后续保存不热启用。
- HTTP 无 target 的测试只证明 TCP，SOCKS5 无 target 证明方法/认证协商；有 target 才执行 CONNECT，不硬编码公网测试站。
- set-follow 的 HTTP 200 表示绑定提交，目录校验异步，GET/events 判断实际应用。active 且 follow_source_dir 为空表示等待 OSC7。
- 保留旧 challenge 不带 ID 的受控当前 pending 入口；新客户端必须带展示轮次 ID。错轮/重复/取消为 CONFLICT，过期为专用机器码，裁剪/重启后 404。

## 实际验证结果

以下保留首次实现后的历史验收记录（审阅修复后的最终结果见末节）。均在 Linux 受控环境执行，退出码均为 0：

```bash
GOWORK=off GOCACHE=/tmp/knot-core-gap-build go test -count=1 -timeout=5m ./...
GOWORK=off GOCACHE=/tmp/knot-core-gap-build go test -race -count=1 -timeout=10m ./...
GOWORK=off GOCACHE=/tmp/knot-core-gap-build go vet ./...
GOCACHE=/tmp/knot-core-gap-build bash ../scripts/build.sh --all
```

| 验证 | 结果 / 证据 |
| --- | --- |
| 全量测试 | 通过，/tmp/knot-core-gap-all.log；integration 包 64.198s，包含生产 60 秒期限 |
| 全量 race | 通过，无 race，/tmp/knot-core-gap-race.log；integration 包 95.131s |
| 历史定向 race（执行参数 -count=10） | 身份/挑战/来源切换与容量/迟到校验、真实 L/R/D/自动去重/IPv6、代理取消、原文并发导入通过，/tmp/knot-core-gap-race-repeat.log；该聚合日志仅显示包级 ok，单凭日志不能核验轮数。新逐测试证据见末节 |
| 公开文件演示 | 真实 HTTP→SFTP 的 literal、链接和 rename 冲突通过，/tmp/knot-core-gap-file-api.log；最终全量/race 已包含该场景 |
| vet / 格式 | vet、gofmt 检查与 git diff --check 通过 |
| 六目标构建 | linux/darwin/windows × amd64/arm64，CGO=0 通过，/tmp/knot-core-gap-build.log；Windows 两个 .exe 已生成 |

构建产物位于 knot-next/bin/knot-core-{linux,darwin,windows}-{amd64,arm64}，Windows 带 .exe。版本仍为 0.1.0-dev、基线 commit=709b462、dirty=true，未进行版本发布。测试全部使用临时 Layout、静态 crypto、人工凭据、本机随机端口，没有写入真实用户业务配置或调用真实用户 SSH。

公开协议演示入口：[gaps](../examples/gaps/README.md)、[baseline](../examples/baseline/README.md)。核心集成场景在 tests/integration/*_gap_test.go；fixture 只准备受控远端和装配服务，场景通过 HTTP/WS 调用，未修改 core 私有 map 伪造端到端状态。领域单元测试以时钟/entropy/阻塞点验证错误、冲突和资源所有权。

## 已说明的边界

1. 六目标构建是编译证据；macOS/Windows 原生 Keychain/DPAPI、Agent、ACL、真实终端未在本轮执行，留到正式 CLI 阶段。真实 OpenSSH 环境联测也未替代受控 server 证据。
2. x/crypto/ssh 的 pending channel/remote-listen 请求不能本地撤回。公开 timeout/stop 超时只限制等待，实际 late worker 继续持有 lease/slot；protocol reply 或 transport 结束后才释放，shutdown 预算不足明确报告。
3. rename overwrite=false 能检测已经存在的目标，并使用服务端普通 rename；没有跨所有 SFTP/本机平台的原子 no-replace 保证。目录验证仍有完整 ReadDir 成本，取消后的句柄 Close 可能等待协议；远端递归的链接循环/权限仍按实际传输结果处理。
4. 新 source_path 写语义是导入。旧磁盘 SourcePath-only 的读取不自动转换；显式重导入后才摆脱源文件。新转发 bind/enabled 等扩展超出旧 loader 的表达能力，回退须恢复 .pre-forwards.bak，不能静默忽略。
5. 全部运行 ID 属于当前 core instance；切换实例/重启或历史裁剪后的 404 表示结果不可查询。前端完整快照应替换旧 DTO，omitempty 省略的已清空字段不能留下旧关联。

## 代码审阅修复后的验收（2026-10-14）

已逐项核验 Q1–Q40，修改和不扩展的理由见 [审阅文档末尾修改报告](../../../docs/running/core缺口补齐开发计划-代码审阅.md#九逐项核验与修改报告2026-10-14)。修复草稿秘密跨地址复用、裁剪订阅二次关闭、挑战内部时钟/受理/断连识别、路由与跟随状态、自动/单流转发错误、配置导入和字段详情、代理协议兼容性与文件错误分类，并同步契约文档。

默认挑战仍为 60 秒；测试装配采用不可变的每实例 Options 缩短到期用例，不再改全局期限。加密 PKCS#8 明确返回 UNSUPPORTED_PRIVATE_KEY；TCP/代理握手的断连观察有界且保留原文，跳板 channel 不支持读 deadline 时仍保留等待期限边界。此前审阅指出的 sshpool 创建完成通知早于临时前缀引用归还已修复，panic 和真实 late worker 的引用所有权继续保留。

| 验证 | 修复后最终结果 |
| --- | --- |
| 全量测试 | 通过，`/tmp/knot-core-review-all.log`；integration 5.497s |
| 全量 race | 通过，无 DATA RACE，`/tmp/knot-core-review-race.log`；integration 31.419s |
| 定向十轮 race | 6 个包 21 个完整测试各 10 次，共 210 次 pass；`/tmp/knot-core-review-repeat.jsonl` 逐测试事件可核 |
| 路由/观察三十轮 race | 6 个完整测试各 30 次，共 180 次 pass；`/tmp/knot-core-review-route-repeat.jsonl`（不是整个 sshpool 包 30 轮） |
| vet / 格式 | go vet、gofmt -l、git diff --check 通过 |
| 六目标编译 | linux/darwin/windows × amd64/arm64 全部通过，CGO=0；`/tmp/knot-core-review-build.log` |

公开验收新增真实远端拒绝、正式 SSH 经两类代理、挑战取消/错资源/认证过期/已观察断连、跳板 role/route/单项查询/显式转发、16 槽容量 HTTP 409、无 posix-rename 冲突。领域回归覆盖订阅裁剪后二次 cancel、自动失败容量、单流事件、并发同源 follow、跟随清理容量、精确 lease 计数不变、TOML 原文内联导入、私钥容器错误分类、字段位置、代理临时响应/超长头和提示原文缓存。

跨进程重启、真实 OpenSSH 与原生 macOS/Windows 运行验证未执行。持久定义有新 config.Service 从磁盘加载证据，自动启用有实际新 transport 的公开 HTTP 测试；不将这两项说成真实进程重启测试。

## 四项残留风险处置后的验证（2026-10-14）

R1 保留有界观察停止，新增恰好 64 KiB 后关闭 peer/继续握手原文的边界回归，并在 sessions API 中明确满额后不再识别等待期 EOF。R2 将导入、旧路径认证和身份摘要统一到 internal/keyutil.ReadSource，旧相对路径仅通过薄适配保留 working-directory 语义；普通文件/1 MiB 上限一致。R3 保留 IP/localhost bind 校验，迁移文档说明 hostname bind 的兼容性收窄及 GET/PUT 修正步骤。R4 改为带连接/定义标识的 forward.auto_failed 独立告警，无空 ID View/无效事件 URL。

详见[审阅文档末尾第十一节](../../../docs/running/core缺口补齐开发计划-代码审阅.md#十一r1r4-残留风险处置报告2026-10-14)。本轮全量测试与全量 race 通过，integration 分别 5.002s / 31.383s；定向 4 包 8 个完整测试每个 10 次，80 次 pass、0 fail，逐测试证据为 /tmp/knot-core-residual-repeat.jsonl。全量日志为 /tmp/knot-core-residual-all.log、/tmp/knot-core-residual-race.log；vet、gofmt、git diff --check 和六目标编译通过，构建日志 /tmp/knot-core-residual-build.log。原生 macOS/Windows 运行边界不变。
