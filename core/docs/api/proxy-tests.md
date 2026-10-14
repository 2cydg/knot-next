# 代理测试

## HTTP 接口

`POST /v1/proxy-tests` 是一次有期限的探测，不创建持久配置或通用 task。

```json
{
  "proxy_id": "prx_saved",
  "draft": {
    "type": "socks5",
    "host": "127.0.0.1",
    "port": 1080,
    "username": "deploy"
  },
  "target": "example.invalid:22",
  "timeout_ms": 15000
}
```

`proxy_id` 和 `draft` 至少提供一个。仅 ID 使用已存普通字段及 secret；draft 是完整的普通字段快照，可同时携带 ID。`draft.password` 省略/null 时仅在协议类型、host 字符串、port 和 username 均与已存档案相同时复用已存密码，显式 `""` 在本次测试清空；修改协议、地址或用户名都不复用旧 secret；需要向新地址认证时显式提供本次 password。测试不会保存参数或修改配置。

`target` 可选，必须为 host:port，支持 `[IPv6]:port`，主机最多 255 bytes。core 不选择隐含公网目标。`timeout_ms=0`/省略为 15 秒，允许 0..60000 毫秒（正值为显式期限）；字段/目标非法为 400，ID 不存在为 404。并发 socket 探测最多 16 个，容量不足为 409；shutdown 后调用为 503 CAPABILITY_UNAVAILABLE。请求取消、期限和 core shutdown 关闭探测自己的 socket。

## 结果模型

正常完成 HTTP 200，即使探测失败也返回结果：

```json
{
  "passed": false,
  "scope": "tunnel",
  "stages": [
    {"name": "endpoint", "passed": true, "duration_ms": 1},
    {"name": "tunnel", "passed": false, "duration_ms": 2}
  ],
  "failure_code": "PROXY_AUTH_FAILED"
}
```

| scope | 实际范围 |
| --- | --- |
| endpoint | 无 target 的 HTTP，仅 TCP 可达；未证明认证或 CONNECT |
| negotiation | 无 target 的 SOCKS5，方法选择及需要的 username/password 协商；未证明目标可达 |
| tunnel | SOCKS5 CONNECT 或 HTTP CONNECT 已向指定目标执行；成功仅证明建隧道，不验证 SSH/应用协议 |

失败机器码为 `PROXY_UNREACHABLE`、`PROXY_NEGOTIATION_FAILED`、`PROXY_AUTH_FAILED`、`PROXY_TARGET_FAILED`、`PROXY_TEST_TIMEOUT`、`PROXY_TEST_CANCELED`、`PROXY_RESPONSE_TOO_LARGE`。结果不返回凭据或任意代理响应文案；SSH 拨号与测试共用协商、认证、期限及取消实现。

HTTP CONNECT 跳过有界 1xx 临时响应，最终任意 2xx 表示隧道成功；完整响应头总量最多 64 KiB（包括临时响应），超限明确返回 PROXY_RESPONSE_TOO_LARGE。单行可超过 4096 bytes，但仍受总量限制。已定型的协议失败码不会被随后发生的 context 取消覆盖。
