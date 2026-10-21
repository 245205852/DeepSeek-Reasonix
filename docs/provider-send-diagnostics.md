# Provider send protection (1.x)

Each provider HTTP attempt has a 300-second send-stage budget. It covers
connection acquisition and request writing, including a new stage after a
redirect or Transport retry. A successful `WroteRequest` hands protection to
the existing response-header timeout. An early final response also ends the
send budget. Response-body streaming retains its existing idle protection;
there is no 300-second limit on a healthy stream.

The timer cancels only the attempt context. It does not set socket deadlines
or close a multiplexed HTTP/2 connection. A send timeout means the body never
finished uploading, so the provider cannot have answered it: the request is
resent once, under the same single-resend allowance as a stale idle pooled
connection, after idle connections are closed. A `request_send_timeout` on the
resend fails the turn. Explicit user cancellation and the outer task deadline retain
priority and are never resent. Worst case before the turn fails is two send
budgets (600 seconds).

## Reading diagnostics

`FailureDiagnostic` retains its existing field structure. New safe kinds are
`request_send_timeout`, `transport_eof`, `transport_reset`, `transport_timeout`
and `transport_dns`. HTTP/2 errors retain `transport_protocol` and
`transportCode`. Unknown causes retain the generic presentation.

Optional `diagnostic/provider` schema version 2 adds `errorCode`, `errorPhase`,
`timeoutSource`, `sendTimeoutMs`, and `sendElapsedMs` to request observations.
Older readers skip this optional event; older records without the fields remain
valid. The send elapsed value describes the latest send stage, not total model
runtime. `requestBytes` is declared body length, **not bytes actually uploaded**.
Missing `writtenAt` means successful write completion was not observed, not
that no bytes were sent. Normal response-body EOF is not a transport failure.
Error prose, bodies, credentials, and URL query parameters are not diagnostic
fields. Records still pass through JSON-aware secret redaction.

## 中文说明

每次模型 HTTP 请求的发送阶段最多等待 300 秒，覆盖等待连接、建立连接与
写入请求。重定向或 Transport 内部重发会重新启用保护。写入成功后由原有
响应头超时接管；提前收到最终响应也会解除发送计时器。正常 SSE 长流继续
使用原有空闲保护，不受发送预算限制。保护只取消本次请求，不关闭共享的
HTTP/2 连接。发送超时说明请求正文未上传完，服务端不可能已作答，因此与“空闲连接已失效”共用同一次
重发额度：关闭空闲连接后重发一次，重发仍超时才判定本轮失败（最坏 600 秒）。用户停止
和外层任务预算优先，且不会触发重发。

必需恢复记录的字段结构不变。新增原因与阶段证据存入可选的
`diagnostic/provider` 第 2 版事件，旧客户端可以跳过。`requestBytes` 是声明的
正文长度，不是实际上传量；缺少 `writtenAt` 不能解释为完全没有上传。
诊断只记录白名单原因码，不保存完整错误文本、正文、密钥或 URL 查询参数。
