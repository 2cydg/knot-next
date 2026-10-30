# 旧配置复用与显式导入

新 core 直接使用旧 Knot 的 `config.toml` 和加密材料。正常升级停止旧 daemon 后启动 core 即可，无需先调用 migration apply，也无需转换 JSON、重新编号或重新录入凭据。

## 原位置与兼容行为

| 数据 | core 的行为 |
| --- | --- |
| 配置 | `~/.config/knot/config.toml`；指定 `--config` 时使用指定文件 |
| 最近使用 | `~/.local/state/knot/state.json`；指定 `--config` 时位于配置文件所在目录的 `.knot-state/state.json` |
| 信任 | 显式 `known_hosts_path` 优先，否则配置目录中的 `known_hosts`；保留 hashed host 和非默认端口记录 |
| 密文 | 保留 `ENC:` + 单层 Base64；provider 接收原始密文字节；普通写入不重新加密未修改的密文 |
| Linux | 复用 `.salt`、`.crypto-state`，固定已选 provider；Secret Service 身份为 `knot` / `knot-master-key`，fallback 为旧 machine-id + NUL + UID 派生 |
| macOS | 复用旧 Keychain 身份 `knot` / `knot-master-key` 与原 machine fallback；不删除或替换损坏的既有 Keychain item |
| Windows | 保持旧 DPAPI flags=1、无额外 entropy，以及原 machine fallback；DPAPI 绑定原 Windows 账号 |
| 持久 forwards | 保留并兼容映射，目标 transport 首次取得时自动启用；扩展字段/备份及旧 loader 边界见 api/forwards.md |
| 客户端偏好 | 当前完整保留；客户端配置拆分在客户端阶段处理 |
| 默认同步 provider | TOML 保存 alias；HTTP 返回/接受 ID，读写时映射 |
| core 发现文件 | state 目录下 `runtime/core.json` 和 `runtime/token`；不覆盖旧 daemon 的 PID、socket、发现文件 |

HTTP/WS 管理接口继续使用 JSON，`schema_version` 和 `updated_at` 属于 API 元数据，不是旧 TOML 的升级要求。core 使用与旧版一致的 `config.toml.lock`，读操作依赖原子文件快照，不创建业务文件；新旧 daemon 不应同时管理同一安装。

未知 TOML 字段允许读取；会丢失未知字段的修改和导入会被拒绝。已有 JSON 业务配置（包括旧 `knot-core` 目录中的另一份 JSON）会被报告并保留；JSON 导入尚未实现，不影响旧 TOML 升级。

## status / plan

`GET /v1/config/migration` 返回 `source: "legacy-toml"`、`legacy_path`、`legacy_exists`、`needs_migration`、`conflict_detected` 和 `json_exists`。默认原位置 TOML 的 `needs_migration` 为 false。额外来源与已有目标共存表示可显式导入，不表示 active TOML 必须转换。

`GET /v1/config/migration/plan` 报告 `reuse` / `preserve`，说明 forwards 尚未启用、JSON 不支持和 recent 中未知 server ID 的数量。只读预览不会初始化或修复 salt、state、平台 key，也不会覆盖 trust/history。普通业务读取无需 apply。

首次启动或合法旧 Linux 安装缺少 `.crypto-state` 时，正式初始化按旧 bootstrap 的候选范围验证所有密文，再固定 provider；只有旧条件确实要求切换 backend 时才重新加密。错误身份、缺失盐、锁定或损坏材料会报错并保留原数据，不尝试 machine-id-only 的弱旧派生。

## 额外 TOML 导入

预览：

```text
GET /v1/config/migration/plan?source_path=/absolute/path/extra.toml&mode=fail_on_conflict
```

响应除脱敏 `items` 外，还返回 `source_revision`、`target_revision` 和按资源分类的 `id_mappings`。默认策略为 `fail_on_conflict`。应用必须带预览返回的两个 revision：

```json
{
  "source_path": "/absolute/path/extra.toml",
  "mode": "skip_existing",
  "source_revision": "<preview source hash>",
  "target_revision": "<preview target hash>"
}
```

发送到 `POST /v1/config/migration/apply`。`target_revision` 在目标不存在时为 `absent`。来源或目标变化返回 409 CONFLICT，原件保留；重复提交旧 revision 也返回冲突。

| 策略 | 冲突行为 |
| --- | --- |
| fail_on_conflict | 任一 ID / alias 冲突拒绝提交 |
| skip_existing | 保留目标对象；源引用映射到匹配的目标 ID |
| overwrite | 更新匹配对象；alias 匹配时复用目标 ID，重映射 key/proxy/jump/default provider 引用 |

ID 与 alias 分别命中不同目标对象属于歧义冲突，三种策略都拒绝。所有来源资源先解密、校验、映射，秘密用目标 provider 加密后再提交；已有目标密文保持不变。备份为 `config.toml.import.bak`，保存精确原件，然后通过锁和原子替换提交单个 TOML。解密、加密、校验、备份或替换失败不会写入半份配置。

额外导入只合并业务 TOML，不自动合并/复制来源 trust、recent 或平台加密材料；来源文件保持原样，`id_mappings` 明确提供引用映射。来源 SourcePath 是 core 所在机器的路径；扩展字段 `source_path`、`fingerprint`、`encrypted` 可在新 core 中保留，旧加载器忽略它们，旧版不能使用只有 SourcePath 的新 key。

有口令 OpenSSH 和传统加密 PEM 均可导入。未解锁 PEM 保留来源 type/length 作为未验证提示，fingerprint 留空，设置 `encrypted=true`；提供口令时才验证私钥内容。预览不索取或保存口令。来源默认同步 alias 不存在时，导入为空默认项并通过 plan 的 `warnings` 报告；原位置读取通过配置 metadata 的 `warnings` 报告，均不改写来源。

没有 `source_path` 的旧 apply 请求保留兼容入口：原位置复用为只读返回 summary；自定义目标目录时使用发现的旧 TOML 进行即时预览并按当前 revision 提交。需要绑定用户审阅结果时使用显式来源和 revision。

## 中断与回退

正常升级不搬移 trust/history。确需同时提交 backend 迁移的 TOML 和 crypto state 时，core 先保存 `config.toml.bootstrap.bak`，再发布仅含加密 TOML/探针的 `.bootstrap-recovery` 标记；下次启动先完成恢复。若 config 或 crypto state 已被外部编辑，恢复返回冲突，不覆盖新编辑。

回退时先停止 core。正常读写后旧版可直接加载 TOML 和原凭据材料；需要恢复显式导入或 backend 迁移前的状态时使用对应备份并保持配套加密材料。恢复标记尚存在时，应先完成或检查该事务。

验证包含旧实现生成的固定密文、只读文件 hash、旧加载器实际回读、真实 HTTP→SSH/SFTP 私钥认证与信任检查，以及子进程强制退出后的 bootstrap 恢复。macOS / Windows 原生凭据库互操作尚未执行；交叉构建仅证明编译通过。

新 secrets source_path 写入已改为原文导入；旧磁盘 SourcePath-only key 仍保留兼容读取，首次显式重新导入才清引用，不在 GET 自动迁移。转发扩展配置写入前保存 config.toml.pre-forwards.bak；回退旧版本应恢复备份，不能让旧 loader 静默忽略 bind/enabled 等新语义。

## 私钥文件读取与 bind 校验的兼容性收窄

旧 SourcePath-only 私钥仍不自动改成内联材料，旧相对路径仍相对于 core 工作目录解析。但认证、身份摘要和新导入现在共用普通文件/1 MiB 上限检查，手写配置引用设备、FIFO、目录或超限文件会被拒绝；新导入额外要求原输入为绝对路径。原位置只读配置不会因这项加固而改写文件。

扩展 forward 的 bind 现在只接受有效 IP（IPv6 可带 zone）或精确的 `localhost`，不接受 `myhost:8080` 等 hostname bind。这相对于此前仅检查 host:port 的手写配置是兼容性收窄，保留它是为了在保存和创建实例前判定绑定地址，不隐含选择 DNS 解析出的接口。

只读加载及 GET 仍可原样显示已有 hostname bind，但后续配置写入的最终校验、新运行实例创建和显式导入会拒绝它；自动启用时会留下失败结果，不静默改写或声称已启用。可先通过 GET/PUT forwards 整组把 bind 改为需要绑定的具体 IP（仅本机访问可用 `127.0.0.1`、`[::1]` 或 `localhost`），也可在停止 daemon 后编辑 TOML。其他普通配置写入须在修正后进行。target 仍接受合法 hostname，此收窄不适用于转发目标地址。

此前通过 XDG 环境变量使用自定义目录的用户，需要自行将原数据迁至新默认目录，或使用 `--config` 指向原配置文件。显式配置文件的状态目录位于其所在目录的 `.knot-state`。
