package dingtalk

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reasonix/internal/bot"
	"reasonix/internal/config"
)

func newTestHTTPClient() *http.Client {
	return &http.Client{}
}

func testAdapter(cfg config.DingtalkBotConfig) *adapter {
	return &adapter{
		cfg:        cfg,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		seen:       make(map[string]bool),
		webhooks:   make(map[string]string),
		msgChats:   make(map[string]string),
		httpClient: newTestHTTPClient(),
	}
}

func TestNormalizeDirectMessage(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{})
	msg := a.normalizeMessage(robotMessage{
		SenderStaffID:    "user-1",
		SenderNick:       "张三",
		ConversationID:   "cid-123",
		ConversationType: "1",
		MsgID:            "msg-1",
		MsgType:          "text",
		Text:             &robotTextContent{Content: "你好"},
		SessionWebhook:   "https://webhook/1",
	})
	if msg == nil {
		t.Fatal("direct message should be accepted")
	}
	if msg.Platform != bot.PlatformDingtalk {
		t.Fatalf("platform = %q, want dingtalk", msg.Platform)
	}
	if msg.ChatType != bot.ChatDM {
		t.Fatalf("chat type = %q, want dm", msg.ChatType)
	}
	if msg.ChatID != "cid-123" || msg.UserID != "user-1" || msg.Text != "你好" {
		t.Fatalf("unexpected message fields: %+v", msg)
	}
	if msg.UserName != "张三" {
		t.Fatalf("user name = %q, want 张三", msg.UserName)
	}
}

func TestNormalizeGroupStripsMention(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{BotName: "我的助手"})
	msg := a.normalizeMessage(robotMessage{
		SenderStaffID:    "user-1",
		SenderNick:       "李四",
		ConversationID:   "cid-grp",
		ConversationType: "2",
		MsgID:            "msg-g1",
		MsgType:          "text",
		Text:             &robotTextContent{Content: "@我的助手 今天天气如何"},
		SessionWebhook:   "https://webhook/2",
	})
	if msg == nil {
		t.Fatal("group message mentioning the bot should be accepted")
	}
	if msg.ChatType != bot.ChatGroup {
		t.Fatalf("chat type = %q, want group", msg.ChatType)
	}
	if msg.Text != "今天天气如何" {
		t.Fatalf("text = %q, want mention stripped", msg.Text)
	}
}

func TestNormalizeGroupRequiresMention(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{RequireMention: true, BotName: "我的助手"})
	// 未 @ 机器人 → 拒绝。
	plain := a.normalizeMessage(robotMessage{
		ConversationID:   "cid-grp",
		ConversationType: "2",
		MsgID:            "msg-g2",
		Text:             &robotTextContent{Content: "普通消息"},
	})
	if plain != nil {
		t.Fatal("group message without @bot should be rejected when require_mention is set")
	}
	// 单个 @ 机器人 → 剥离后为空文本。
	only := a.normalizeMessage(robotMessage{
		ConversationID:   "cid-grp",
		ConversationType: "2",
		MsgID:            "msg-g3",
		Text:             &robotTextContent{Content: "@我的助手"},
	})
	if only == nil || only.Text != "" {
		t.Fatalf("bare mention should pass gating with empty text, got %+v", only)
	}
}

func TestNormalizeDeduplicatesByMsgID(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{})
	first := a.normalizeMessage(robotMessage{
		ConversationID: "cid-1", ConversationType: "1", MsgID: "dup-1",
		Text: &robotTextContent{Content: "hi"},
	})
	if first == nil {
		t.Fatal("first delivery should be accepted")
	}
	second := a.normalizeMessage(robotMessage{
		ConversationID: "cid-1", ConversationType: "1", MsgID: "dup-1",
		Text: &robotTextContent{Content: "hi"},
	})
	if second != nil {
		t.Fatal("duplicate msgId should be dropped")
	}
}

func TestNormalizeMissingIDsRejected(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{})
	if msg := a.normalizeMessage(robotMessage{ConversationID: "cid", ConversationType: "1", MsgID: ""}); msg != nil {
		t.Fatal("empty msgId should be rejected")
	}
	if msg := a.normalizeMessage(robotMessage{ConversationID: "", ConversationType: "1", MsgID: "m"}); msg != nil {
		t.Fatal("empty chatId should be rejected")
	}
}

func TestSendRequiresSessionWebhook(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{ClientID: "id", ClientSecret: "secret"})
	_, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID: "cid-1",
		Text:   "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "session webhook") {
		t.Fatalf("send without webhook should fail with a clear error, got %v", err)
	}
}

// TestNormalizeRecordsSessionWebhook: normalize 必须把 webhook 记入
// chatID→webhook 映射表，并透传到 InboundMessage。
func TestNormalizeRecordsSessionWebhook(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{})
	msg := a.normalizeMessage(robotMessage{
		ConversationID:   "cid-web",
		ConversationType: "1",
		MsgID:            "msg-w1",
		Text:             &robotTextContent{Content: "hi"},
		SessionWebhook:   "https://webhook/learned",
	})
	if msg == nil {
		t.Fatal("message should be accepted")
	}
	if msg.SessionWebhook != "https://webhook/learned" {
		t.Fatalf("inbound session_webhook = %q, want learned value", msg.SessionWebhook)
	}
	if got := a.webhookFor("cid-web"); got != "https://webhook/learned" {
		t.Fatalf("webhook map = %q, want learned value", got)
	}
}

// TestSendUsesLearnedWebhook: 入站学习到 webhook 后，sendMessage 应 POST 到
// 该 webhook 而非 ReplyToMsgID（gateway 会把 ReplyToMsgID 填成消息 ID）。
func TestSendUsesLearnedWebhook(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-acs-dingtalk-access-token")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	// 预置 token 缓存，避免测试发起真实 gettoken 请求。
	a.token = "test-token"
	a.tokenAt = time.Now()
	// 入站消息学习 webhook。
	if m := a.normalizeMessage(robotMessage{
		ConversationID: "cid-learn", ConversationType: "1", MsgID: "m1",
		Text: &robotTextContent{Content: "hi"}, SessionWebhook: srv.URL,
	}); m == nil {
		t.Fatal("inbound message should be accepted")
	}
	// 出站：ReplyToMsgID 是消息 ID（非 URL），必须忽略并查表。
	if _, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID:       "cid-learn",
		ChatType:     bot.ChatDM,
		Text:         "回复",
		ReplyToMsgID: "om-12345", // 模拟 gateway 填入的消息 ID
	}); err != nil {
		t.Fatalf("send via learned webhook failed: %v", err)
	}
	if !strings.Contains(gotBody, "回复") {
		t.Fatalf("webhook body = %q, want reply text", gotBody)
	}
	if gotAuth != "test-token" {
		t.Fatalf("access token header = %q, want test-token", gotAuth)
	}
}

// TestSendPrefersExplicitURLWebhook: ReplyToMsgID 为显式 http(s) URL 时优先使用
// （桌面端测试发送场景），不必先收到入站消息。
func TestSendPrefersExplicitURLWebhook(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	if _, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID:       "cid-x",
		ChatType:     bot.ChatDM,
		Text:         "显式发送",
		ReplyToMsgID: srv.URL,
	}); err != nil {
		t.Fatalf("send with explicit webhook URL failed: %v", err)
	}
	if !strings.Contains(gotBody, "显式发送") {
		t.Fatalf("webhook body = %q, want explicit text", gotBody)
	}
}

// TestSendPrefersSessionWebhookOverLearned: 入站消息透传的 SessionWebhook
// 优先于映射表（gateway 重启后持久化恢复场景，adapter 内存映射可能为空）。
func TestSendPrefersSessionWebhookOverLearned(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	// 映射表里是旧 webhook，透传的是新 webhook，必须用新的。
	a.webhooks["cid-x"] = "https://old.example.com/hook"
	if _, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID:         "cid-x",
		ChatType:       bot.ChatDM,
		Text:           "透传发送",
		SessionWebhook: srv.URL,
	}); err != nil {
		t.Fatalf("send with session webhook failed: %v", err)
	}
	if !strings.HasPrefix(gotPath, "/") {
		t.Fatalf("request path = %q, want httptest path", gotPath)
	}
}

// TestSendPlainTextUsesMarkdown: 普通文本（无 Card）也必须以 markdown 类型
// 发送，否则钉钉按纯文本显示、不渲染 markdown 语法（与飞书一致）。
func TestSendPlainTextUsesMarkdown(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	if _, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID:       "cid-md",
		ChatType:     bot.ChatDM,
		Text:         "**加粗** 和 `code`",
		ReplyToMsgID: srv.URL,
	}); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if !strings.Contains(gotBody, `"msgtype":"markdown"`) {
		t.Fatalf("plain text send must use markdown msgtype, got %s", gotBody)
	}
	if !strings.Contains(gotBody, "**加粗** 和 `code`") {
		t.Fatalf("markdown body should carry original text, got %s", gotBody)
	}
}

// TestSendMarkdownCard: Card 存在时发送 markdown 消息。
func TestSendMarkdownCard(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	_, err := a.sendMessage(context.Background(), bot.OutboundMessage{
		ChatID:       "cid-card",
		ChatType:     bot.ChatDM,
		Text:         "**bold** content",
		ReplyToMsgID: srv.URL,
		Card:         &bot.InteractiveCard{Header: "标题"},
	})
	if err != nil {
		t.Fatalf("send markdown card failed: %v", err)
	}
	if !strings.Contains(gotBody, `"msgtype":"markdown"`) || !strings.Contains(gotBody, "**bold**") {
		t.Fatalf("webhook body = %q, want markdown payload", gotBody)
	}
}

// TestDecodeRobotMessageNestedJSONData: Stream 回调的 data 是 JSON 编码的
// 字符串，decodeRobotMessage 必须先解码字符串再解析消息（与 dsh transport.ts
// 的 JSON.parse(res.data) 一致），同时兼容 data 直接是对象的情况。
func TestDecodeRobotMessageNestedJSONData(t *testing.T) {
	inner, err := json.Marshal(robotMessage{
		SenderStaffID:    "user-nested",
		SenderNick:       "嵌套",
		ConversationID:   "cid-nested",
		ConversationType: "1",
		MsgID:            "msg-nested",
		MsgType:          "text",
		Text:             &robotTextContent{Content: "双层"},
		SessionWebhook:   "https://webhook/nested",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 双层编码：data 是 JSON 字符串，其内容为 robotMessage 的 JSON。
	nestedBytes, err := json.Marshal(string(inner))
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := decodeRobotMessage(json.RawMessage(nestedBytes))
	if !ok {
		t.Fatal("nested string payload should decode")
	}
	if raw.MsgID != "msg-nested" || raw.ConversationID != "cid-nested" || raw.SessionWebhook != "https://webhook/nested" {
		t.Fatalf("unexpected decoded message: %+v", raw)
	}

	// 直接对象（兼容路径）。
	direct, ok := decodeRobotMessage(json.RawMessage(inner))
	if !ok {
		t.Fatal("direct object payload should decode")
	}
	if direct.MsgID != "msg-nested" {
		t.Fatalf("direct payload msg id = %q", direct.MsgID)
	}

	// 非法载荷。
	if _, ok := decodeRobotMessage(json.RawMessage(`"not-json`)); ok {
		t.Fatal("garbage payload should be rejected")
	}
}

func TestSplitMentionBotNameMismatch(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{BotName: "我的助手"})
	mentionsBot, rest := a.splitMention("@别人 你好")
	if mentionsBot {
		t.Fatal("mention of another user should not count as @bot")
	}
	if rest != "@别人 你好" {
		t.Fatalf("mismatched mention must keep the original text, got %q", rest)
	}
}

func TestClientCredentialsFromEnv(t *testing.T) {
	t.Setenv("DINGTALK_TEST_ID", "env-id")
	t.Setenv("DINGTALK_TEST_SECRET", "env-secret")
	a := testAdapter(config.DingtalkBotConfig{
		ClientIDEnv: "DINGTALK_TEST_ID",
		SecretEnv:   "DINGTALK_TEST_SECRET",
	})
	if got := a.clientID(); got != "env-id" {
		t.Fatalf("client id = %q, want env-id", got)
	}
	if got := a.clientSecret(); got != "env-secret" {
		t.Fatalf("client secret = %q, want env-secret", got)
	}
}

// TestTestSendWithoutKnownChat: 还没有任何交互过的会话时，测试发送返回
// 可读错误，而不是发起真实请求。
func TestTestSendWithoutKnownChat(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{})
	if _, err := a.TestSend(context.Background(), "hi"); err == nil {
		t.Fatal("TestSend without a known chat should fail")
	} else if !strings.Contains(err.Error(), "requires a known chat") {
		t.Fatalf("error = %q, want readable known-chat hint", err.Error())
	}
}

// TestTestSendUsesLatestLearnedChat: 测试发送会发到最近交互过的会话
// （normalizeMessage 学到 webhook 后记录 lastChatID）。
func TestTestSendUsesLatestLearnedChat(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := testAdapter(config.DingtalkBotConfig{})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	if m := a.normalizeMessage(robotMessage{
		ConversationID: "cid-latest", ConversationType: "1", MsgID: "m1",
		Text: &robotTextContent{Content: "hi"}, SessionWebhook: srv.URL,
	}); m == nil {
		t.Fatal("inbound message should be accepted")
	}
	if _, err := a.TestSend(context.Background(), "测试消息"); err != nil {
		t.Fatalf("TestSend failed: %v", err)
	}
	if !strings.Contains(gotBody, "测试消息") {
		t.Fatalf("webhook body = %q, want test text", gotBody)
	}
}

// TestAddPendingReactionPinsAndRecallsEmotion: 收到消息后 AddPendingReaction
// 贴 🤔思考中 表情，cleanup 撤回；emotion 请求体携带 robotCode/chat/message。
func TestAddPendingReactionPinsAndRecallsEmotion(t *testing.T) {
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		actions = append(actions, r.URL.Path+"|"+string(b))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	oldURL := emotionURL
	emotionURL = srv.URL + "/v1.0/robot/emotion"
	t.Cleanup(func() { emotionURL = oldURL })

	a := testAdapter(config.DingtalkBotConfig{ClientID: "ding-appkey", ClientSecret: "secret"})
	a.httpClient = srv.Client()
	a.token = "test-token"
	a.tokenAt = time.Now()
	// 入站消息学习 chat。
	if m := a.normalizeMessage(robotMessage{
		ConversationID: "cid-emotion", ConversationType: "1", MsgID: "msg-emotion-1",
		Text: &robotTextContent{Content: "hi"}, SessionWebhook: "https://webhook/emotion",
	}); m == nil {
		t.Fatal("inbound message should be accepted")
	}
	cleanup, err := a.AddPendingReaction(context.Background(), "msg-emotion-1")
	if err != nil {
		t.Fatalf("AddPendingReaction failed: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup must not be nil")
	}
	cleanup()

	if len(actions) != 2 {
		t.Fatalf("expected 2 emotion calls (reply+recall), got %d: %v", len(actions), actions)
	}
	if !strings.Contains(actions[0], "/reply") || !strings.Contains(actions[0], "🤔思考中") {
		t.Fatalf("first call should be reply with thinking emotion, got %q", actions[0])
	}
	if !strings.Contains(actions[0], "ding-appkey") || !strings.Contains(actions[0], "cid-emotion") || !strings.Contains(actions[0], "msg-emotion-1") {
		t.Fatalf("reply body missing robotCode/chat/message: %q", actions[0])
	}
	if !strings.Contains(actions[1], "/recall") {
		t.Fatalf("second call should be recall, got %q", actions[1])
	}
}

// TestAddPendingReactionUnknownMessage: 未记录过的 messageID 报可读错误。
func TestAddPendingReactionUnknownMessage(t *testing.T) {
	a := testAdapter(config.DingtalkBotConfig{ClientID: "ding-appkey", ClientSecret: "secret"})
	if _, err := a.AddPendingReaction(context.Background(), "unknown-msg"); err == nil {
		t.Fatal("unknown message should fail")
	} else if !strings.Contains(err.Error(), "unknown chat") {
		t.Fatalf("error = %q, want unknown-chat hint", err.Error())
	}
}
