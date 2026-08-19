# 在 Reasonix 上支持钉钉 IM 机器人 — 工作清单

> 分支：`pr/feishu-chat-session-reuse`（worktree: `.worktrees/feishu-chat-session-reuse`）
> 范围：把 Reasonix 现有的 IM bot 网关（QQ / 飞书 / 微信）扩展到钉钉，
> 参考 DSH 的 [dsh-dingtalk-channel](https://github.com/ttmouse/dsh-dingtalk-channel) 实现。
> 前置：本文假设"飞书会话复用"改造（确定性会话路径）已落地——钉钉接入直接继承该机制。

---

## 1. 现状：Reasonix bot 网关架构

```
飞书/QQ/微信 adapter (internal/bot/{feishu,qq,weixin}/)
        │  实现 bot.Adapter 接口（Start/Stop/Send/SendTyping/Messages）
        ▼
BotGateway (internal/bot/gateway.go)
        │  消息路由、allowlist、SessionManager 并发控制
        ▼
getOrCreateSession → boot.Build(controller) → 会话文件（确定性路径 bot-<key>.jsonl）
```

- Adapter 接口：`internal/bot/types.go:173`
  ```go
  type Adapter interface {
      Platform() Platform
      Start(ctx context.Context) error
      Stop() error
      Send(ctx context.Context, msg OutboundMessage) (SendResult, error)
      SendTyping(ctx context.Context, chatID string) error
      Messages() <-chan InboundMessage
      Name() string
  }
  ```
- 平台常量：`PlatformQQ` / `PlatformFeishu` / `PlatformWeixin`（`types.go:14`）
- 配置：`BotConfig.QQ/Feishu/Weixin` + 通用 `Connections []BotConnectionConfig`（provider 字符串）
- 绑定：`botruntime.AdapterBindings`（`internal/botruntime/runtime.go:306`）按 provider 分发
- 会话身份：`BuildSessionKey`（`internal/bot/session.go:92`）按 DM/group/thread 派生确定性 hash

---

## 2. 钉钉接入的架构差异（关键）

| 维度 | 飞书 / QQ | 钉钉（dsh-dingtalk-channel 模式） |
|---|---|---|
| 连接方式 | SDK WebSocket / 官方 gateway | **Stream 模式**：`dingtalk-stream` 的 DWClient WebSocket 长连接，无需公网回调 |
| 发送回复 | 全局 API：`client.Im.Message.Create(chat_id)` | **无全局发送 API**：每条入站消息携带 `sessionWebhook`，回复必须 POST 到该 webhook（带 `x-acs-dingtalk-access-token`） |
| 消息类型 | text/image/file/post/card | text / markdown（经 webhook） |
| "正在输入" | SendTyping（QQ 不支持） | 无 typing 指示器；用 **emoji（🤔思考中）** 加/撤（`/v1.0/robot/emotion/reply|recall`） |
| 群聊 @ 识别 | mentions 列表 / require_mention 门控 | 平台只转发 @bot 的消息；需剥离前导 `@昵称` token（`botName` 可配） |
| 会话 id | —（本次改造为确定性路径） | `ding-<chatId>`（chat 模式）/ `ding-<chatId>:<senderId>`（chat-sender 模式） |

> **核心差异 → 适配器必须维护 `chatID → sessionWebhook` 映射表**：adapter 从入站消息学习
> webhook，`Send(ctx, msg)` 时按 `msg.ChatID` 查表回发。**无法主动触达未交互过的会话**
> （如 `/desktop watch` 推送、审批卡片提醒到从未发言的群 → 无 webhook 可用，需降级/跳过）。

---

## 3. 工作清单

### 3.1 新增 adapter 包 `internal/bot/dingtalk/`

仿照 `internal/bot/feishu/` 结构：

| 文件 | 内容 |
|---|---|
| `dingtalk.go` | `New(cfg, logger) bot.Adapter`；`Platform() = PlatformDingTalk`；`Start` 启动 DWClient 长连接 + 重连；`Stop` 优雅断开 |
| `inbound.go` | `TOPIC_ROBOT`（`/v1.0/im/bot/messages/get`）消息归一化：conversationId → ChatID、conversationType 1/2 → ChatDM/ChatGroup、senderId → UserID、msgId → MessageID、剥离群聊前导 @token、记录 sessionWebhook |
| `outbound.go` | `Send`：按 ChatID 查 webhook → POST `{msgtype: text|markdown}`（`x-acs-dingtalk-access-token` 头）；`SendTyping` → 映射为 emotion 加/撤（可选实现，失败静默） |
| `config.go`（或并入） | 读取 `config.DingTalkBotConfig` |

依赖：`go get github.com/open-dingtalk/dingtalk-stream-sdk-go`（官方 Go Stream SDK，与 npm `dingtalk-stream` 对应）。

### 3.2 平台常量与类型

- `internal/bot/types.go`：加 `PlatformDingTalk Platform = "dingtalk"`；`BotSelfUserIDs`/allowlist 等平台枚举视需要加 dingtalk 字段（可先用 `Connections` 通用路径，allowlist 复用 `BotAccessConfig`）。

### 3.3 配置层（`internal/config/config.go`）

- 加 `DingTalkBotConfig`：
  ```toml
  [bot.dingtalk]
  enabled = true
  client_id_env = "DINGTALK_CLIENT_ID"      # 或直接 client_id
  client_secret_env = "DINGTALK_CLIENT_SECRET"
  bot_name = "Reasonix"                     # 群聊 @ 剥离
  require_mention = true                    # 群聊仅 @ 响应
  session_scope = "chat"                    # chat | chat-sender（默认 chat，共享群内一个 agent）
  ```
- `BotConfig` 加 `DingTalk DingTalkBotConfig` 字段 + 默认值（`Default()` 处）。
- `BotConnectionConfig.Provider` 注释与校验支持 `"dingtalk"`；`BotConnectionCredential` 复用 `AppID/AppSecretEnv` 字段（钉钉 = ClientID/ClientSecret）。

### 3.4 botruntime 分发（`internal/botruntime/runtime.go`）

所有按 platform 分发的 switch 加 `PlatformDingTalk` 分支（现有 31 处 `PlatformQQ` 引用点涉及）：
- `EnabledPlatforms` / `PlatformConfigured`（channels 列表支持 `dingtalk`）
- `AdapterBindings`：`feishu.New` 同级加 `dingtalk.New(cfg, logger)`，`ConnectionRuntimeID` 默认 `dingtalk`（无 domain 时）
- `ChannelConfigs` / `ConnectionChannelConfigs` / `RouteConfigs`（model/workspace/tool_approval_mode 透传）
- `ModelName` 回退链
- `rememberAllowlist` / `AllowlistUserCount`（若走通用 access 则可能无需改）

### 3.5 会话复用（继承本次飞书改造，钉钉免费获得）

- 确定性会话路径 `bot-<BuildSessionKey>.jsonl` 与平台无关 → 钉钉同 chat 自动复用同一对话。
- 群聊隔离语义：`BuildSessionKey` 对 group 已按 `chat+user` 隔离；钉钉 `session_scope = "chat"` 时需**覆盖**为按 chat 共享（整群一个 agent），或明确映射：`chat` → `ChatDM` 式按 chat；`chat-sender` → 现有 group 行为。
  - 建议：钉钉 `session_scope=chat` 时把消息 ChatType 映射为 `ChatDM`（按 chat 隔离）；`chat-sender` 保持 `ChatGroup`（按 user 隔离），或给 `BuildSessionKey` 加 scope 参数。

### 3.6 桌面端（`desktop/`）

- `bot_connection_app.go`：连接安装流程加 dingtalk（输入 ClientID/Secret、测试发送——测试目标需要已有 webhook 的会话，或仅校验 token）。
- `settings_app.go` / `metrics_app.go`：视图与指标加 dingtalk 字段（`settings_bot_dingtalk_enabled` 等）。
- `bot_runtime_app.go`：`desktopBotChannelsWithLegacyQQ` 等平台枚举处加 dingtalk。

### 3.7 CLI（`internal/cli/bot.go`）

- `--channels` 解析：`splitBotChannels` 支持 `dingtalk`；`RequestedFeishuDomains` 类逻辑不涉及。

### 3.8 测试

- adapter 单测：webhook 归一化、@剥离、outbound 查表回发（httptest 桩 webhook）。
- gateway 集成：钉钉两条同 chat 消息 → 同一会话文件、turns 递增（复用确定性路径测试框架）。
- 跨重启：同 chat 消息 resume 同一文件。
- 群聊：chat-sender 每人一会话 / chat 整群一会话。

---

## 4. 增量工作量评估

| 项 | 量级 | 说明 |
|---|---|---|
| `internal/bot/dingtalk/` | 中（~600-900 行） | 主体工作量；inbound 归一化 + webhook 查表 outbound |
| config / botruntime 分支 | 小（~100-150 行） | 机械加 switch 分支 |
| 桌面端接入 | 中 | 连接安装 UI + 指标 |
| 会话复用对齐 | 小 | session_scope 语义映射 |
| 测试 | 中 | adapter + gateway 集成 |
| 文档 | 小 | BOT_GUIDE 补钉钉章节 |

总计约 **1.5–2 天有效工作量**（不含评审/联调），其中钉钉 SDK 联调（Stream 长连接 + webhook 发送 + emotion）是最不确定的部分。

---

## 5. 风险与注意点

1. **webhook 表是内存态**：adapter 重启后丢失；只有收到新入站消息才能重建。主动通知（desktop watch、审批提醒）到从未发言的会话会失败 → 需记录并向用户提示或静默降级。
2. **钉钉 markdown 限制**：webhook markdown 仅支持部分语法（不支持表格/代码块高亮的平台差异），渲染器需适配（参考 dsh-dingtalk `renderer.ts`）。
3. **@ 剥离可靠性**：`botName` 未配置时仅剥离首个 `@\S+` token，可能与用户正文冲突；建议配置 `bot_name`。
4. **Stream SDK 稳定性**：官方 Go SDK 需验证断线重连、事件 ack 语义（`socketCallBackResponse`），对齐 `transport.ts:179-192` 的 ack 即回、handler 去重的模式。
5. **emoji 接口**：`/v1.0/robot/emotion/reply|recall` 需 `robotCode`（= clientId）与 `emotionId` 常量，失败应静默不影响主流程。
6. **与飞书改造的协作**：钉钉接入应在 `pr/feishu-chat-session-reuse` 合并后基于新基线进行，避免在同一分支堆叠两个大改动。

---

## 6. 参考实现对照

| 参考 | 位置 | 借鉴点 |
|---|---|---|
| dsh-dingtalk-channel | `src/transport.ts` | DWClient 长连接、access token 缓存、webhook 发送、emotion、ack 即回 |
| dsh-dingtalk-channel | `src/message.ts` | 消息归一化（conversationType→isGroup、@剥离、sessionWebhook 透传） |
| dsh-dingtalk-channel | `src/session.ts` | 确定性会话 id（`ding-<chatId>` / `chat-sender`）——与本仓库确定性路径方案同构 |
| dsh-dingtalk-channel | `src/authorization.ts` | senderAllowlist / groupAllowlist / requireMention → Reasonix `BotAccessConfig` |
| Reasonix feishu adapter | `internal/bot/feishu/` | adapter 骨架、gateway 接入方式、测试模式 |
