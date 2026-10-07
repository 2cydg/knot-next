# Streaming

core 使用 HTTP/JSON 管理资源，使用 WebSocket 订阅变化或承载交互字节流。连接需要与 HTTP 相同的 Bearer token；浏览器连接还受 Origin 策略约束。
SSH attach 的 PTY 字节流直接转发，不应用以下 JSON 事件处理规则。

## SFTP 传输快照和事件

连接 `GET /v1/sftp/{session_id}/transfers/events` 后，第一个 text frame 是：

```json
{
  "type": "sftp.transfer.snapshot",
  "session_id": "sftp_1",
  "transfers": []
}
```

`transfers` 包含该 session 当前保留的完整 Transfer，包括已完成任务；字段见 [SFTP API](api/sftp.md)。
空列表始终编码为 `[]`。订阅注册和快照读取在同一个服务锁内完成，后续变化通过实时事件发布。

后续 text frame 是 TransferEvent。例如：

```json
{
  "type": "sftp.transfer.completed",
  "session_id": "sftp_1",
  "transfer_id": "transfer_2",
  "direction": "upload",
  "state": "completed",
  "bytes_total": 4,
  "bytes_copied": 4,
  "files_total": 1,
  "files_done": 1,
  "current_path": "/file.txt",
  "error": "",
  "time": "2026-10-07T00:00:00Z"
}
```

以下四种状态都结束任务等待：`completed`、`failed`、`partial_failed`、`canceled`。
部分失败事件为 `sftp.transfer.partial_failed`，需要按失败结果展示各项目错误。
服务端每个订阅使用有界缓冲；慢消费者可能丢失进度或终态事件。事件提供通知，HTTP GET 提供最终资源结果。

## 晚订阅和断线恢复

以上传为例：

1. POST `/v1/sftp/{id}/upload`，保存 202 响应的 transfer ID。
2. 建立 transfer 事件连接，在首帧的 transfers 中查找该 ID。任务即使已经完成也可能直接出现在首帧。
3. GET `/v1/sftp/{id}/transfers/{transfer_id}` 确认最新状态。如果已是任一终态，立即结束等待。
4. 运行中处理事件，并保留有界 GET 兜底。测试客户端可每 250 ms 查询一次、单次总等待上限 5 秒；这是测试策略，不是协议规定的生产超时。
5. WS 断线后继续 GET，或重连后重新读取快照；使用原 transfer ID，不重新提交文件传输。
6. 观察终态后，不让缓冲中的旧 queued/started/progress 事件把任务恢复成 running。

例如，任务完成后才建立连接，首帧包含 `state=completed`，GET 确认结果后便可结束；无需等待另一个 completed 事件。
如果终态通知因消费者缓慢而丢失，GET 仍可查询完成状态。关闭 session 可能先关闭事件流，关联任务收尾同样通过 GET 查询。

任务仅保留在当前进程内，历史任务可能被清理。core 重启或任务被清理后，GET 返回 404 时应报告“结果已不可查询”，不能当作成功或继续无限等待。

## 取消与后端关闭

DELETE `/v1/sftp/{id}/transfers/{transfer_id}` 返回取消时的快照，取消由后台 worker 异步收尾。
重复取消已结束任务不改变终态或完成时间。批量任务保留此前成功和失败的项目，当前中断项目及未启动项目收敛为 canceled。
已写入的数据不会自动回滚。

普通任务取消会在下一次可执行的 context 检查处生效，不能保证立即中断永久阻塞的远端 I/O。
关闭 session 时先发出任务取消信号，再关闭该 SFTP client，使该连接上正在进行的读写返回并收尾。
执行函数使用锁内捕获的后端视图，SSH 会话关闭后不会切换为本地文件后端。

## 测试连接期限

测试 WebSocket 辅助函数为拨号设置 2 秒超时，为握手、首帧和默认事件读写设置 5 秒期限；定向终态事件等待使用 2 秒 read deadline。
循环检查墙钟只能限制循环次数，无法中断阻塞读取；需要在实际 net.Conn 上设置 deadline，并关闭连接、取消订阅。
这些期限仅约束测试，不是生产 WebSocket 的自动关闭策略。
