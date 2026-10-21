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

## Automatic protocol compatibility

For the built-in Chat Completions, Responses and Anthropic HTTP clients, a
typed HTTP/2 `INTERNAL_ERROR`, `PROTOCOL_ERROR` or `HTTP_1_1_REQUIRED` records a
temporary compatibility hint. The failed request still returns its original
error. It is **not replayed**, because the peer may already have processed it.
No tool execution or agent turn is restarted by this mechanism.

Only a subsequent new request tries HTTP/1.1. Hints are isolated by configured
provider identity, adapter, exact request endpoint and the actual resolved proxy
route. One concurrent request probes an unproven route; after response headers
arrive, subsequent requests may use HTTP/1.1. A transport failure on that route
disables compatibility until the hint expires, including failures while reading
the response body. A failed compatibility request never consumes the existing
send-timeout/stale-connection resend allowance. User cancellation does not mark
the route incompatible. HTTP status errors retain their existing handling.

Hints expire 10 minutes after the original HTTP/2 failure and are capped at 256
entries. They survive provider reconstruction in the same process, but not an
application restart. Expiry returns to normal protocol negotiation; it does not
start a background request. Custom injected clients and credential tunnels keep
their existing transport contracts. TLS verification, proxy routing, request
bodies, model selection and prompt-cache inputs are unchanged. Other HTTP/2
streams continue on their original connection.

Request diagnostics expose the actual `httpProtocol` and the allowlisted
`protocolFallbackReason` when compatibility was selected. The latter is a hint
from an earlier request, not the current error code. Optional
`diagnostic/provider` events use schema version 2; required recovery records and
session schema versions are unchanged. Old observations simply omit the field,
and older session readers skip the optional event. No URL, proxy credential or
raw error text is added by compatibility tracking.

This is bounded compatibility handling, not proof that HTTP/2 or DeepSeek was
the original cause. It cannot guarantee that an upstream or network fault will
disappear over HTTP/1.1.

## 中文说明

每次模型 HTTP 请求的发送阶段最多等待 300 秒，覆盖等待连接、建立连接与
写入请求。重定向或 Transport 内部重发会重新启用保护。写入成功后由原有
响应头超时接管；提前收到最终响应也会解除发送计时器。正常 SSE 长流继续
使用原有空闲保护，不受发送预算限制。保护只取消本次请求，不关闭共享的
HTTP/2 连接。发送超时说明请求正文未上传完，服务端不可能已作答，因此与“空闲连接已失效”共用同一次
重发额度：关闭空闲连接后重发一次，重发仍超时才判定本轮失败（最坏 600 秒）。用户停止
和外层任务预算优先，且不会触发重发。

内置模型客户端遇到明确的 HTTP/2 `INTERNAL_ERROR`、`PROTOCOL_ERROR` 或
`HTTP_1_1_REQUIRED` 时，当前请求仍然失败，不重放请求、不重新执行工具。
只有后续新请求才尝试 HTTP/1.1；选择按供应商、协议适配器、请求端点和实际
代理路径隔离，保留 10 分钟。兼容请求发生传输失败时，不再进入发送超时或
空闲连接重发分支，并暂停该路径的兼容尝试直到过期。用户取消不算协议失败。
正常共享 HTTP/2 流不受影响。诊断中的 `protocolFallbackReason` 表示此前触发
兼容选择的原因，`httpProtocol` 表示实际响应协议；这不等于已经确认现场根因。
