# 飞书 IM 机器人：每条消息独立对话 — 问题定位与改造方案

> 分支：`pr/feishu-chat-session-reuse`（worktree: `.worktrees/feishu-chat-session-reuse`）
> 目标：让飞书 IM 机器人在 Reasonix 上**复用同一对话**（同一 chat 的所有消息进同一个会话），
> 参考 DSH 的飞书（dsh-lark-channel）与钉钉（dsh-dingtalk-channel）实现。

---

## 1. 问题现象

用户在飞书上向 Reasonix IM 机器人（`@reasonix`）每发送一条消息，Reasonix 侧就产生一个**新的对话/会话**，
而不是把后续消息并入已有会话。表现为：

- Reasonix 会话列表中一个 chat 对应多个会话文件；
- 每个会话只有 1 个 turn（第一条消息进去后，后续消息不会续接）；
- 配置中 `session_mappings` 被反复覆盖/追加，`updated_at` 频繁变动。

### 实机证据（~/.reasonix/config.toml）

```toml
[[bot.connections]]
id = "feishu-feishu"
provider = "feishu"
domain = "feishu"
enabled = true
session_mappings = [
  { remote_id = "oc_1f0f852e258479b797d1cfd402811c33",
    scope = "global",
    session_id = "path:/Users/douba/.reasonix/sessions/20260813-104246.276024000-deepseek-v4-flash.jsonl",
    session_source = "auto", updated_at = "2026-08-13T10:42:46Z" },
  { chat_type = "group",
    remote_id = "oc_0747406b86320189a5602cdeb84ec005",
    scope = "global",
    session_id = "path:/Users/douba/.reasonix/projects/-Users-douba-.reasonix-global-workspace/sessions/20260816-041307.083323000-deepseek-v4-flash.jsonl",
    session_source = "auto", updated_at = "2026-08-16T04:13:09Z",
    user_id = "ou_6de1300af2f0d4b2b0f6569b9229bc0d" }
]
```

会话文件名形如 `20260813-104246.276024000-deepseek-v4-flash.jsonl`：
**前缀是 UTC 纳秒时间戳**，每个新会话都不同 → 这是"每条消息一个对话"的直接来源。

---

## 2. 根因定位（代码级）

### 2.1 会话文件命名是"时间戳随机"，不是"chat 确定性派生"

- `internal/agent/save.go:2282` — `NewSessionPath(dir, model)`
  ```go
  return filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl",
      time.Now().UTC().Format("20060102-150405.000000000"), safe))
  ```
  每次调用产生一个**全新且不可预测**的文件名，与 chat/会话身份完全无关。

- 飞书 bot 的会话路径由 `internal/control/sessionpath.go:19` 的 `EnsureSessionPath()` 决定：
  ```go
  func (c *Controller) EnsureSessionPath() {
      if c.SessionPath() != "" || c.SessionDir() == "" { return }
      c.SetFreshSessionPath(agent.NewSessionPath(c.SessionDir(), c.Label()))
  }
  ```
  只要 controller 还没有路径，就生成一个新的时间戳文件。

### 2.2 Gateway 的"会话复用"依赖持久化 mapping，但 mapping 只在事后写入

- `internal/bot/gateway.go:2259` `getOrCreateSession`：
  1. `sessionProfileForMessage(msg)` 先查 **override**（`/attach` 等），再查 **持久化 mapping**
     （`sessionMappingPathForMessage`，从 `gw.cfg.ConnectionChannels[*].SessionMappings` 匹配 remote_id）。
  2. 命中 mapping → `agent.LoadSession` + `ctrl.Resume` → **复用旧文件** ✅
  3. 未命中 → 新 controller → `EnsureSessionPath()` → **新时间戳文件** ❌

- `internal/bot/gateway.go:2445` `sessionMappingPathForMessage` 只读**内存快照**
  `gw.cfg.ConnectionChannels`，它是在 gateway 启动时由 `botruntime.ConnectionChannelConfigs`
  （`internal/botruntime/runtime.go:143`）从 config 构建的。

- 运行时新增的 mapping 由 `OnSessionReady` 回调写入：
  `gateway.go:2571` `rememberSessionPath` → `botruntime.rememberInbound`
  （`internal/botruntime/runtime.go:513`）——它 **只写磁盘 config 文件**，
  **不回写 gateway 内存中的 ConnectionChannels**。

> **核心矛盾**：会话复用决策读的是"启动时快照"，而 mapping 的更新写的是"磁盘 config"。
> 两者不同步 → 只要 gateway 启动后第一个新 chat 的消息到来，必然 miss mapping → 新文件；
> 之后的 mapping 更新只进磁盘，内存仍 miss → **每条消息都新建会话文件**。

### 2.3 每条消息都成为新对话的完整链路

```
飞书消息 → feishu adapter (internal/bot/feishu/feishu.go:401) 构造 InboundMessage
        → gateway.handleMessage (gateway.go:715)
        → key = BuildSessionKey(msg.Session())          // 同一 chat 的 key 稳定（sha256(chat_id)）
        → runTurn → getOrCreateSession (gateway.go:2259)
        → sessionProfileForMessage:
            override? 无
            sessionMappingPathForMessage? 读内存快照 ConnectionChannels
              ├─ 有 mapping 且文件存在 → Resume 复用 ✅（仅当 gateway 启动时 config 已有该映射）
              └─ 无（新 chat / 运行期新增映射未回流内存）→ sessionPath = ""
        → boot.Build 新 controller
        → EnsureSessionPath() → NewSessionPath → 新时间戳文件  ❌ 每消息一个对话
        → rememberSessionReady → rememberInbound 把新文件写进磁盘 config mapping
```

### 2.4 为什么"参考 DSH 就能修"

DSH 的飞书（`dsh-lark-channel`）/ 钉钉（`dsh-dingtalk-channel`）channel 采用
**确定性会话 id**，见 `dsh-dingtalk-channel/src/session.ts`：

```ts
const SESSION_PREFIX = 'ding-'
export function conversationKey(scope, msg) {
  // chat / chat-thread → msg.chatId
  // chat-sender → `${msg.chatId}:${msg.senderId}`
}
export function sessionIdFor(key) { return `${SESSION_PREFIX}${key}` }  // ding-<chatId>
```

- **会话 id = f(chat 身份)**：同一 chat 永远派生同一 session id，跨进程、跨重启稳定；
- 宿主侧 `lookup → resume → create` 三档爬梯（`host.ts` / `bridge.ts`），
  不存在"每消息建新会话"的路径；
- 群聊可配 `sessionScope: chat-sender` 让"每人在共享群里各一个 agent"，
  语义与 Reasonix `BuildSessionKey` 的 group 按 user 隔离一致。

Reasonix 的问题正是**反模式**：会话身份 = 时间戳随机值 + 事后 mapping 追认，
而 mapping 又只在磁盘、不进内存。

---

## 3. 改造方案

### 3.1 目标

让 bot 会话文件路径由 chat 身份**确定性派生**，同一 chat（DM 或 group+user）的
所有消息命中同一会话文件；同时保留 `/attach`、`/new` 等显式控制的语义。

### 3.2 推荐方案：确定性 bot 会话路径（对齐 DSH）

#### A. 新增确定性路径派生函数（`internal/bot/` 新文件 `session_path.go`）

```go
// BotSessionPathForChat 由 chat 身份确定性派生 bot 会话路径，跨重启稳定。
// 命名对齐 DSH channel 的 "前缀 + 会话键" 模式：
//   dm    -> bot-<conn>-dm-<chatId>
//   group -> bot-<conn>-group-<chatId>-<userId>
//   thread-> bot-<conn>-thread-<threadId>
func BotSessionPathForChat(sessionDir string, src SessionSource) string {
    key := BuildSessionKey(src) // 已实现：sha256(platform:conn:chat_type:chat_id[:user_id])[:16]
    return filepath.Join(sessionDir, "bot-"+key+".jsonl")
}
```

要点：
- 复用现有 `BuildSessionKey`（`internal/bot/session.go:92`）——它已经是 chat 身份
  的确定性 hash，正好是"会话键"；只把 hash 换成文件名即可。
- 前缀 `bot-` 避免与时间戳命名/用户手动会话冲突，且一眼可识别。
- **不引入新配置**：完全由消息身份派生，天然跨重启稳定。

#### B. Gateway 侧优先使用确定性路径

修改 `internal/bot/gateway.go`：

1. `sessionProfileForMessage`（gateway.go:2414）：
   - override（`/attach` 显式指定）优先级**不变**（用户显式绑定时尊重用户）；
   - 持久化 mapping 命中且文件存在 → 仍优先（向后兼容已有映射）；
   - **都未命中 → fallback 到 `BotSessionPathForChat(botSessionDir(workspaceRoot), msg.Session())`**，
     而不是空串。

2. `getOrCreateSession`（gateway.go:2259）：
   - 当 `profile.sessionPath` 来自确定性派生时，文件不存在 = **首次消息**，
     用 `ctrl.SetSessionPath(path)` 绑定（而不是 `EnsureSessionPath()` 生成时间戳文件）；
   - 文件已存在 = **后续消息**，走现有 `agent.LoadSession` + `ctrl.Resume` 分支复用。

3. `EnsureSessionPath()` 对 bot 会话不再产生时间戳文件：
   - 在 bot 的创建路径中先 `SetSessionPath(确定性路径)` 再 `EnsureSessionPath()`，
     `EnsureSessionPath` 检测到已有路径即 no-op（`sessionpath.go:15` 已有该语义）。

#### C. mapping 持久化保留，作为"桌面展示/显式绑定"的辅助

- `rememberInbound`（`internal/botruntime/runtime.go:513`）继续写磁盘 mapping，
  但 `session_id` 现在是**确定性路径**（`bot-<key>.jsonl`），同一 chat 不再反复覆盖；
- 可选优化：mapping 命中即复用，miss 则不再需要它来兜底（确定性路径已兜底），
  因此**即使 mapping 与内存不同步，也不会再产生孤儿会话**。

#### D. 清理历史孤儿文件（可选，工具/脚本）

- 扫描 `session_mappings` 未指向的、`bot-` 前缀 / 时间戳命名的孤儿会话，
  提供一次性清理命令（或先人工核对后删除）。

### 3.3 兼容性

| 关注点 | 影响 |
|---|---|
| 已有持久化 mapping（8/13、8/16 两个） | 保留；mapping 命中继续复用旧文件；新消息 fallback 到确定性路径，随后 mapping 更新为确定性路径 |
| `/attach <session>` 显式绑定 | 优先级最高，不受影响（override 分支先行） |
| `/new`、`/reset` | `/new` 语义（开新会话）需确认：确定性路径下 `/new` 若仍写同一文件则无"新会话"效果 → 建议 `/new` 时把映射目标切换到新确定性路径（版本号后缀 `bot-<key>-<n>.jsonl`）或保留时间戳分支给显式 `/new` |
| 群聊每 user 独立会话 | 已由 `BuildSessionKey` 的 `group:chat:user` 语义继承 |
| thread 会话 | `ChatThread` 分支已按 thread_id 派生，继承 |
| 恢复（recovery）路径 | `botSessionRecoveredHandler`（gateway.go:2590）会在恢复时更新 `state.sessionPath` 与 mapping——确定性路径下恢复目标也应保持确定性，需同步处理 |
| 桌面端展示 | `desktop/app.go:3101 channelSessionRoutesForDir` 读 mapping 的 `session_source == "auto"` 展示 channel 会话——确定性路径下同一 chat 只有一条 mapping，展示更干净 |

### 3.4 实施步骤

1. 新增 `internal/bot/session_path.go`：`BotSessionPathForChat` + 单测
   （同 chat 派生同路径、不同 chat 不同路径、跨"进程重启"稳定）。
2. 修改 `internal/bot/gateway.go` `sessionProfileForMessage`：
   未命中 override/mapping 时返回确定性路径，并标记 `sessionPathDeterministic=true`。
3. 修改 `internal/bot/gateway.go` `getOrCreateSession`：
   deterministic 分支先 `os.Stat` 判存在 → 存在走 Resume，不存在走 SetSessionPath。
4. 处理 `/new` 语义与 recovery 路径（见 3.3 兼容性表）。
5. `rememberInbound`：确定 session_id 写入不变，但值变为确定性路径；
   顺带修复内存/磁盘不同步（可选：OnSessionReady 后回写内存 ConnectionChannels）。
6. 测试：
   - 单元：`BotSessionPathForChat` 确定性/隔离性；
   - gateway 集成：同 chat 两条消息 → 同一 session 文件、turns 递增；
   - 跨重启：重启 gateway 后同 chat 消息 → resume 同一文件；
   - 群聊：同群不同 user → 不同会话；同群同 user → 同一会话。
7. 清理测试产生的孤儿会话。

### 3.5 备选方案（不推荐，仅记录）

- **仅修复内存/磁盘不同步**：让 `rememberInbound` 成功后同步更新
  `gw.cfg.ConnectionChannels`。能解决"运行期 miss"问题，但会话文件仍是时间戳随机，
  跨重启后 mapping 需逐条追认，新 chat 首条消息仍可能 miss；根因未除。
- **改用会话 id 注册表**（类似 DSH 的 agent registry）：改动面大，涉及桌面端
  会话列表/展示联动，超出本次 PR 范围。

---

## 4. 参考实现速览

| 参考 | 位置 | 关键设计 |
|---|---|---|
| dsh-dingtalk-channel | `src/session.ts` `conversationKey` / `sessionIdFor` | 会话 id = `ding-<chatId>` 确定性派生；`ConversationSessions` 按 key 去重绑定 |
| dsh-dingtalk-channel | `src/host.ts` / `src/bridge.ts` | 宿主侧 `lookup → resume → create` 三档爬梯；`sessionId` 是 agent registry 的持久化键 |
| dsh-lark-channel | README 所述（omdsh-dev/dsh-lark, BSD-3-Clause） | 同一 channel 设计：窄宿主契约 + 传输层端口 + 会话爬梯 + 事件渲染 |
| Reasonix 现状 | `internal/bot/session.go` `BuildSessionKey` | 已有 chat 身份 → 确定性 hash 的"会话键"，缺的是把它变成文件路径 |

---

## 5. 结论

- **根因**：bot 会话文件由 `agent.NewSessionPath` 以纳秒时间戳命名（随机），
  会话复用依赖事后写入的 `session_mappings`，而 gateway 判定复用时只读启动时
  内存快照、运行期 mapping 只落磁盘 → 新 chat 每条消息必然 miss → 新文件。
- **修复**：仿照 DSH channel，把 bot 会话路径改为 `bot-<BuildSessionKey>.jsonl`
  的确定性派生，同一 chat 天然复用同一文件；mapping 降级为展示/显式绑定辅助。
- **影响面**：`internal/bot/session_path.go`（新增）+ `internal/bot/gateway.go`
  （`sessionProfileForMessage` / `getOrCreateSession`）+ 少量兼容处理
  （`/new`、recovery、桌面展示），不动飞书 adapter 与配置格式。
