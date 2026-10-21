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

## 中文说明

每次模型 HTTP 请求的发送阶段最多等待 300 秒，覆盖等待连接、建立连接与
写入请求。重定向或 Transport 内部重发会重新启用保护。写入成功后由原有
响应头超时接管；提前收到最终响应也会解除发送计时器。正常 SSE 长流继续
使用原有空闲保护，不受发送预算限制。保护只取消本次请求，不关闭共享的
HTTP/2 连接。发送超时说明请求正文未上传完，服务端不可能已作答，因此与“空闲连接已失效”共用同一次
重发额度：关闭空闲连接后重发一次，重发仍超时才判定本轮失败（最坏 600 秒）。用户停止
和外层任务预算优先，且不会触发重发。
