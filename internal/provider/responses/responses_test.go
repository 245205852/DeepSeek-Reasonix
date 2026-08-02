package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/provider"
)

func boolPtr(value bool) *bool { return &value }

func collect(t *testing.T, p provider.Provider, req provider.Request) []provider.Chunk {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := p.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var chunks []provider.Chunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func writeEvents(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = w.Write([]byte("event: ignored\n"))
		_, _ = w.Write([]byte("data: " + event + "\n\n"))
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func TestDetectVendorAndModeDefaults(t *testing.T) {
	tests := []struct{ url, vendor, mode string }{
		{"https://api.deepseek.com", "deepseek", "stateless"},
		{"https://eu.deepseek.com/v1", "deepseek", "stateless"},
		{"https://api.xiaomimimo.com/v1", "mimo", "stateless"},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "dashscope", "stateful"},
		{"https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "dashscope", "stateful"},
		{"https://api.deepseek.com.attacker.example/v1", "", "stateful"},
		{"https://example.com/api.deepseek.com/v1", "", "stateful"},
		{"https://example.com/v1", "", "stateful"},
	}
	for _, test := range tests {
		if got := DetectVendor(test.url); got != test.vendor {
			t.Errorf("DetectVendor(%q) = %q, want %q", test.url, got, test.vendor)
		}
		if got := (Config{BaseURL: test.url}).mode(); got != test.mode {
			t.Errorf("mode(%q) = %q, want %q", test.url, got, test.mode)
		}
	}
	if got := (Config{BaseURL: "https://api.deepseek.com", Mode: "stateful"}).mode(); got != "stateful" {
		t.Fatalf("explicit mode = %q", got)
	}
	if got := (Config{BaseURL: "https://api.deepseek.com", Mode: "stateful", Stateful: boolPtr(false)}).mode(); got != "stateful" {
		t.Fatalf("mode must win over legacy stateful, got %q", got)
	}
	if got := (Config{BaseURL: "https://example.com", Stateful: boolPtr(false)}).mode(); got != "stateless" {
		t.Fatalf("legacy stateful=false mode = %q", got)
	}
}

func TestDeepSeekEffortUsesResponsesReasoningShape(t *testing.T) {
	tests := []struct{ effort, want string }{
		{"auto", ""}, {"disabled", "none"}, {"minimal", "minimal"}, {"low", "low"}, {"high", "high"}, {"max", "max"},
	}
	for _, test := range tests {
		client := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Effort: test.effort}).(*client)
		body, _, _ := client.buildRequestBody(provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
		reasoning, _ := body["reasoning"].(map[string]any)
		got, _ := reasoning["effort"].(string)
		if got != test.want {
			t.Errorf("effort %q serialized as %q, want %q", test.effort, got, test.want)
		}
	}
}

func TestRequestSerializesExplicitMaxOutputTokens(t *testing.T) {
	client := New(Config{Name: "responses", BaseURL: "https://example.com", Model: "model"}).(*client)
	body, _, _ := client.buildRequestBody(provider.Request{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: 32 * 1024,
	})
	if got := body["max_output_tokens"]; got != 32*1024 {
		t.Fatalf("max_output_tokens = %#v, want 32768", got)
	}
}

func TestRequestUsesOnlySafeProviderOutputDefaults(t *testing.T) {
	message := []provider.Message{{Role: provider.RoleUser, Content: "hi"}}

	deepseek := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	deepseekBody, _, _ := deepseek.buildRequestBody(provider.Request{Messages: message})
	if got := deepseekBody["max_output_tokens"]; got != provider.DefaultReasoningOutputTokens {
		t.Fatalf("DeepSeek max_output_tokens = %#v, want %d", got, provider.DefaultReasoningOutputTokens)
	}

	for _, effort := range []string{"none", "disabled", "off", " NONE "} {
		thinkingDisabled := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Effort: effort}).(*client)
		thinkingDisabledBody, _, _ := thinkingDisabled.buildRequestBody(provider.Request{Messages: message})
		if _, exists := thinkingDisabledBody["max_output_tokens"]; exists {
			t.Fatalf("thinking-disabled DeepSeek effort %q received an automatic output budget: %#v", effort, thinkingDisabledBody)
		}
	}

	explicitThinkingDisabled := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Effort: "none", MaxOutputTokens: 8192}).(*client)
	explicitThinkingDisabledBody, _, _ := explicitThinkingDisabled.buildRequestBody(provider.Request{Messages: message})
	if got := explicitThinkingDisabledBody["max_output_tokens"]; got != 8192 {
		t.Fatalf("explicit thinking-disabled DeepSeek budget = %#v, want 8192", got)
	}

	unknown := New(Config{Name: "responses", BaseURL: "https://example.com", Model: "model"}).(*client)
	unknownBody, _, _ := unknown.buildRequestBody(provider.Request{Messages: message})
	if _, exists := unknownBody["max_output_tokens"]; exists {
		t.Fatalf("unknown Responses endpoint received an inferred output budget: %#v", unknownBody)
	}

	disabled := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", MaxOutputTokens: -1}).(*client)
	disabledBody, _, _ := disabled.buildRequestBody(provider.Request{Messages: message})
	if _, exists := disabledBody["max_output_tokens"]; exists {
		t.Fatalf("disabled DeepSeek Responses budget remained present: %#v", disabledBody)
	}
}

func TestFactoryPreservesUnsetLegacyStatefulForVendorDetection(t *testing.T) {
	p, err := newFromConfig(provider.Config{
		Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash",
		Extra: map[string]any{"stateful": (*bool)(nil)},
	})
	if err != nil {
		t.Fatalf("newFromConfig: %v", err)
	}
	if got := p.(*client).mode; got != "stateless" {
		t.Fatalf("unset stateful mode = %q, want DeepSeek vendor default stateless", got)
	}
}

func TestStatelessRequestReplaysReasoningContentAndToolPair(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}`)
	}))
	defer server.Close()

	p := New(Config{Name: "deepseek", APIKey: "key", BaseURL: server.URL, Model: "deepseek-v4-flash", Mode: "stateless", Effort: "high"})
	collect(t, p, provider.Request{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "weather"},
		{Role: provider.RoleAssistant, Content: "checking", ReasoningContent: "need a tool", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "weather", Arguments: `{"city":"SG"}`}}},
		{Role: provider.RoleTool, ToolCallID: "call_1", Name: "weather", Content: "sunny"},
	}})

	if body["previous_response_id"] != nil {
		t.Fatalf("stateless request has previous_response_id: %#v", body)
	}
	if body["instructions"] != "system" {
		t.Fatalf("instructions = %#v", body["instructions"])
	}
	items, ok := body["input"].([]any)
	if !ok || len(items) != 5 {
		t.Fatalf("input = %#v, want user/reasoning/assistant/call/output", body["input"])
	}
	wantTypes := []string{"", "reasoning", "", "function_call", "function_call_output"}
	for i, want := range wantTypes {
		item := items[i].(map[string]any)
		if got, _ := item["type"].(string); got != want {
			t.Errorf("item[%d].type = %q, want %q: %#v", i, got, want, item)
		}
	}
	assistant := items[2].(map[string]any)
	if assistant["content"] != "checking" {
		t.Fatalf("assistant content lost: %#v", assistant)
	}
	reasoning := items[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if reasoning["type"] != "reasoning_text" || reasoning["text"] != "need a tool" {
		t.Fatalf("reasoning item = %#v", reasoning)
	}
}

func TestStatelessRequestSanitizesMissingToolOutput(t *testing.T) {
	client := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	body, _, _ := client.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "run"},
		{Role: provider.RoleAssistant, ReasoningContent: "call", ToolCalls: []provider.ToolCall{{ID: "call_1", Name: "bash", Arguments: `{"command":"pwd"}`}}},
	}})
	items := body["input"].([]map[string]any)
	if got := items[len(items)-1]["type"]; got != "function_call_output" {
		t.Fatalf("last input item = %#v, want repaired function_call_output", items[len(items)-1])
	}
}

func TestStreamDoesNotDuplicateDoneText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEvents(w,
			`{"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"hello"}`,
			`{"type":"response.output_text.done","item_id":"msg_1","content_index":0,"text":"hello"}`,
			`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":4}}}`,
		)
	}))
	defer server.Close()

	chunks := collect(t, New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateless"}), provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	var text string
	var usage *provider.Usage
	for _, chunk := range chunks {
		if chunk.Type == provider.ChunkText {
			text += chunk.Text
		}
		if chunk.Type == provider.ChunkUsage {
			usage = chunk.Usage
		}
	}
	if text != "hello" {
		t.Fatalf("streamed text = %q, want one copy", text)
	}
	if usage == nil || usage.CacheHitTokens != 2 || usage.CacheMissTokens != 1 || usage.ReasoningTokens != 1 {
		t.Fatalf("usage = %+v", usage)
	}
	if chunks[len(chunks)-1].Type != provider.ChunkDone {
		t.Fatalf("last chunk = %v", chunks[len(chunks)-1].Type)
	}
}

func TestFunctionArgumentEventsUseOutputItemMappingAndCumulativeProgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEvents(w,
			`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"bash"}}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":"}`,
			`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"pwd\"}"}`,
			`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"command\":\"pwd\"}"}`,
			`{"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"pwd\"}"}}`,
			`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}`,
		)
	}))
	defer server.Close()

	chunks := collect(t, New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateless"}), provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "pwd"}}})
	var starts, completed int
	var progress []int
	for _, chunk := range chunks {
		switch chunk.Type {
		case provider.ChunkToolCallStart:
			starts++
			if chunk.ToolCall.ID != "call_1" || chunk.ToolCall.Name != "bash" {
				t.Errorf("start = %+v", chunk.ToolCall)
			}
		case provider.ChunkToolCallArgsDelta:
			progress = append(progress, chunk.ArgChars)
			if chunk.ToolCall.ID != "call_1" {
				t.Errorf("delta ID = %q", chunk.ToolCall.ID)
			}
		case provider.ChunkToolCall:
			completed++
			if chunk.ToolCall.ID != "call_1" || chunk.ToolCall.Name != "bash" || chunk.ToolCall.Arguments != `{"command":"pwd"}` {
				t.Errorf("complete = %+v", chunk.ToolCall)
			}
		}
	}
	if starts != 1 || completed != 1 {
		t.Fatalf("starts=%d completed=%d", starts, completed)
	}
	if len(progress) != 2 || progress[1] <= progress[0] {
		t.Fatalf("argument progress = %v, want cumulative", progress)
	}
}

func TestIncompleteResponseSurfacesFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEvents(w, `{"type":"response.incomplete","response":{"id":"resp_1","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)
	}))
	defer server.Close()
	p := New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateful"}).(*client)
	p.lastResponseID = "stale"
	p.expectedPrefixDigest = "stale"
	chunks := collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	for _, chunk := range chunks {
		if chunk.Type == provider.ChunkUsage {
			if chunk.Usage.FinishReason != "length" {
				t.Fatalf("finish reason = %q", chunk.Usage.FinishReason)
			}
			if p.lastResponseID != "" || p.expectedPrefixDigest != "" {
				t.Fatalf("incomplete response retained stateful context: id=%q digest=%q", p.lastResponseID, p.expectedPrefixDigest)
			}
			return
		}
	}
	t.Fatal("missing usage chunk")
}

func TestStatefulContinuationValidatesConversationPrefix(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		id := len(bodies)
		mu.Unlock()
		writeEvents(w,
			`{"type":"response.output_text.delta","item_id":"msg","delta":"answer"}`,
			`{"type":"response.completed","response":{"id":"resp_`+string(rune('0'+id))+`","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	}))
	defer server.Close()
	p := New(Config{Name: "stateful", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateful"})

	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleSystem, Content: "sys"}, {Role: provider.RoleUser, Content: "one"}}})
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleSystem, Content: "sys"}, {Role: provider.RoleUser, Content: "one"}, {Role: provider.RoleAssistant, Content: "answer"}, {Role: provider.RoleUser, Content: "two"}}})
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleSystem, Content: "different"}, {Role: provider.RoleUser, Content: "new session"}}})

	if bodies[1]["previous_response_id"] != "resp_1" || bodies[1]["input"] != "two" {
		t.Fatalf("valid continuation = %#v", bodies[1])
	}
	if bodies[1]["instructions"] != "sys" {
		t.Fatalf("valid continuation instructions = %#v, want %q", bodies[1]["instructions"], "sys")
	}
	if _, ok := bodies[2]["previous_response_id"]; ok {
		t.Fatalf("session switch reused previous response: %#v", bodies[2])
	}
	if _, ok := bodies[2]["input"].([]any); !ok {
		t.Fatalf("session switch did not send full input: %#v", bodies[2]["input"])
	}
}

func TestExpiredPreviousResponseRetriesOnceWithFullHistory(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()
		if attempt == 2 {
			http.Error(w, `previous_response_id expired`, http.StatusBadRequest)
			return
		}
		id := "resp_1"
		if attempt == 3 {
			id = "resp_2"
		}
		writeEvents(w,
			`{"type":"response.output_text.delta","item_id":"msg","delta":"answer"}`,
			`{"type":"response.completed","response":{"id":"`+id+`","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	}))
	defer server.Close()
	p := New(Config{Name: "stateful", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateful"})
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "one"}}})
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "one"}, {Role: provider.RoleAssistant, Content: "answer"}, {Role: provider.RoleUser, Content: "two"}}})
	if len(bodies) != 3 {
		t.Fatalf("request count = %d, want initial + stale + retry", len(bodies))
	}
	if bodies[1]["previous_response_id"] != "resp_1" {
		t.Fatalf("stale attempt = %#v", bodies[1])
	}
	if _, ok := bodies[2]["previous_response_id"]; ok {
		t.Fatalf("retry still has previous response: %#v", bodies[2])
	}
	if _, ok := bodies[2]["input"].([]any); !ok {
		t.Fatalf("retry input = %#v, want full array", bodies[2]["input"])
	}
}

func TestDashScopeCacheHeaderIsVendorScoped(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-dashscope-session-cache")
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	}))
	defer server.Close()
	p := New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateless", SessionCache: boolPtr(true)}).(*client)
	p.vendor = "deepseek"
	p.caps = capabilitiesFor("deepseek")
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	if got != "" {
		t.Fatalf("DeepSeek request leaked DashScope header %q", got)
	}
	p.vendor = "dashscope"
	p.caps = capabilitiesFor("dashscope")
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	if got != "enable" {
		t.Fatalf("DashScope header = %q", got)
	}
}

func TestVendorCapabilityTableCoversKnownEndpoints(t *testing.T) {
	tests := []struct {
		url, vendor          string
		stateless, reasoning bool
		ignoresTemp          bool
	}{
		{"https://api.deepseek.com", "deepseek", true, true, false},
		{"https://api.xiaomimimo.com/v1", "mimo", true, true, true},
		{"https://dashscope.aliyuncs.com/compatible-mode/v1", "dashscope", false, false, false},
		{"https://example.com/v1", "", false, false, false},
	}
	for _, test := range tests {
		vendor := DetectVendor(test.url)
		if vendor != test.vendor {
			t.Errorf("DetectVendor(%q) = %q, want %q", test.url, vendor, test.vendor)
		}
		caps := capabilitiesFor(vendor)
		if caps.stateless != test.stateless {
			t.Errorf("capabilities(%q).stateless = %v, want %v", vendor, caps.stateless, test.stateless)
		}
		if caps.toolCallReasoning != test.reasoning {
			t.Errorf("capabilities(%q).toolCallReasoning = %v, want %v", vendor, caps.toolCallReasoning, test.reasoning)
		}
		if caps.ignoresTemperature != test.ignoresTemp {
			t.Errorf("capabilities(%q).ignoresTemperature = %v, want %v", vendor, caps.ignoresTemperature, test.ignoresTemp)
		}
	}
}

func TestMiMoOmitsTemperatureFromRequestBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		if err := json.Unmarshal(body, &reqBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, ok := reqBody["temperature"]; ok {
			t.Fatalf("MiMo request must not carry temperature (vendor forces 1.0 in thinking mode), got %#v", reqBody["temperature"])
		}
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	}))
	defer server.Close()

	temp := 0.3
	p := New(Config{Name: "mimo", APIKey: "key", BaseURL: server.URL, Model: "mimo-v2.5-pro"}).(*client)
	p.vendor = "mimo"
	p.caps = capabilitiesFor("mimo")
	if !p.caps.ignoresTemperature {
		t.Fatal("MiMo capability must ignore temperature")
	}
	collect(t, p, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}, Temperature: &temp})

	// Control: an unknown endpoint still sends temperature.
	var gotTemp any
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		_ = json.Unmarshal(body, &reqBody)
		gotTemp = reqBody["temperature"]
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	}))
	defer control.Close()
	p2 := New(Config{Name: "openai", APIKey: "key", BaseURL: control.URL, Model: "m"}).(*client)
	collect(t, p2, provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}, Temperature: &temp})
	if gotTemp == nil {
		t.Fatal("unknown endpoint must still send temperature")
	}
}

func TestRequiresToolCallReasoningForStatelessVendors(t *testing.T) {
	deepseek := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"})
	if !provider.RequiresToolCallReasoning(deepseek) {
		t.Fatal("DeepSeek Responses provider must preserve tool-call reasoning")
	}
	mimo := New(Config{Name: "mimo", BaseURL: "https://api.xiaomimimo.com/v1", Model: "mimo-v2.5-pro"})
	if !provider.RequiresToolCallReasoning(mimo) {
		t.Fatal("MiMo Responses provider must preserve tool-call reasoning (documented requirement)")
	}
	other := New(Config{Name: "other", BaseURL: "https://example.com", Model: "m"})
	if provider.RequiresToolCallReasoning(other) {
		t.Fatal("unknown Responses endpoint unexpectedly requires tool-call reasoning")
	}
}

func TestMissingToolCallReasoningWarningFingerprintTracksResponsesConfiguration(t *testing.T) {
	first := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Effort: "high"})
	same := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com/", Model: "deepseek-v4-flash", Effort: "high"})
	changedEffort := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Effort: "max"})
	got := provider.MissingToolCallReasoningWarningFingerprint(first)
	if got != provider.MissingToolCallReasoningWarningFingerprint(same) {
		t.Fatal("equivalent Responses configurations produced different fingerprints")
	}
	if got == provider.MissingToolCallReasoningWarningFingerprint(changedEffort) {
		t.Fatal("Responses effort change did not re-key the warning fingerprint")
	}
}

func TestWarnOnMissingToolCallReasoningIsModelScoped(t *testing.T) {
	// DeepSeek pro-tier: warns (endpoint reliably emits tool-call reasoning).
	pro := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-pro"})
	if !provider.WarnOnMissingToolCallReasoning(pro) {
		t.Fatal("DeepSeek pro must warn on missing tool-call reasoning")
	}
	// DeepSeek flash-tier: no warning (flash does not emit tool-call reasoning).
	flash := New(Config{Name: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"})
	if provider.WarnOnMissingToolCallReasoning(flash) {
		t.Fatal("DeepSeek flash must not warn on missing tool-call reasoning")
	}
	// MiMo: preserves reasoning on replay but does not guarantee it every
	// round — a missing chain-of-thought is endpoint-conditional, not a
	// degradation worth a warning (observed: mimo-v2.5-pro tool-call turn
	// with empty reasoning).
	mimo := New(Config{Name: "mimo", BaseURL: "https://api.xiaomimimo.com/v1", Model: "mimo-v2.5-pro"})
	if provider.WarnOnMissingToolCallReasoning(mimo) {
		t.Fatal("MiMo must not warn on missing tool-call reasoning (endpoint-conditional)")
	}
	other := New(Config{Name: "other", BaseURL: "https://example.com", Model: "m"})
	if provider.WarnOnMissingToolCallReasoning(other) {
		t.Fatal("unknown Responses endpoint must not warn on missing tool-call reasoning")
	}
}

func TestFailedEventSurfacesAuthenticationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEvents(w, `{"type":"response.failed","response":{"id":"resp","error":{"code":"invalid_api_key","message":"bad API key"}}}`)
	}))
	defer server.Close()
	chunks := collect(t, New(Config{Name: "test", APIKey: "key", KeyEnv: "TEST_API_KEY", BaseURL: server.URL, Model: "m"}), provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	for _, chunk := range chunks {
		if chunk.Type == provider.ChunkError {
			if _, ok := chunk.Err.(*provider.AuthError); !ok || !strings.Contains(chunk.Err.Error(), "TEST_API_KEY") {
				t.Fatalf("error = %T %v", chunk.Err, chunk.Err)
			}
			return
		}
	}
	t.Fatal("missing error chunk")
}

func TestCompletedResponseDefaultsFinishReasonToStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No finish_reason anywhere in the completed event: the client must
		// synthesize FinishReason="stop" for a normally completed response.
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)
	}))
	defer server.Close()
	chunks := collect(t, New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateful"}), provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	for _, chunk := range chunks {
		if chunk.Type == provider.ChunkUsage {
			if chunk.Usage.FinishReason != "stop" {
				t.Fatalf("finish reason = %q, want \"stop\"", chunk.Usage.FinishReason)
			}
			return
		}
	}
	t.Fatal("missing usage chunk")
}

func TestAllZeroUsageCompletedIsSuppressed(t *testing.T) {
	// DashScope occasionally reports a completed event whose usage object is
	// present but all zeros (server-side reporting gap). The client must NOT
	// emit a ChunkUsage for it, or cache-ratio and cost accounting would
	// record a spurious zero-token turn.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeEvents(w, `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	}))
	defer server.Close()
	chunks := collect(t, New(Config{Name: "test", APIKey: "key", BaseURL: server.URL, Model: "m", Mode: "stateful"}), provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}})
	for _, chunk := range chunks {
		if chunk.Type == provider.ChunkUsage {
			t.Fatalf("all-zero usage must be suppressed, got usage chunk: %+v", chunk.Usage)
		}
	}
}

func TestMessagesToInputIncludesSummaryOnReasoningItems(t *testing.T) {
	client := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	body, _, _ := client.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "run"},
		{Role: provider.RoleAssistant, ReasoningContent: "think step by step", Content: "answer"},
	}})
	items := body["input"].([]map[string]any)
	var reasoning map[string]any
	for _, item := range items {
		if item["type"] == "reasoning" {
			reasoning = item
			break
		}
	}
	if reasoning == nil {
		t.Fatal("missing reasoning item in input")
	}
	// DashScope requires summary as a list, not a scalar; OpenAI format only
	// needs content. The serialized input must carry both.
	summary, ok := reasoning["summary"].([]map[string]string)
	if !ok {
		t.Fatalf("reasoning summary = %#v, want []map[string]string list", reasoning["summary"])
	}
	if len(summary) != 1 || summary[0]["type"] != "summary_text" || summary[0]["text"] != "think step by step" {
		t.Fatalf("reasoning summary = %#v", summary)
	}
	content, ok := reasoning["content"].([]map[string]string)
	if !ok || len(content) != 1 || content[0]["type"] != "reasoning_text" || content[0]["text"] != "think step by step" {
		t.Fatalf("reasoning content = %#v", reasoning["content"])
	}
}

func TestMessagesToInputOmitsSummaryWithoutReasoning(t *testing.T) {
	client := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	body, _, _ := client.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "run"},
		// Assistant turn without reasoning: no reasoning item and thus no
		// summary list should be serialized at all.
		{Role: provider.RoleAssistant, Content: "plain answer"},
	}})
	items := body["input"].([]map[string]any)
	for _, item := range items {
		if item["type"] == "reasoning" {
			t.Fatalf("unexpected reasoning item without ReasoningContent: %#v", item)
		}
	}
}

func TestMessagesToInputTextOnlyStaysStringShape(t *testing.T) {
	client := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	body, _, _ := client.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "hello"},
		{Role: provider.RoleAssistant, Content: "hi"},
	}})
	items := body["input"].([]map[string]any)
	// Text-only turns keep the documented TextInput string shape even with
	// vision enabled: images are the only trigger for the array form.
	for i, item := range items {
		if _, isArr := item["content"].([]map[string]string); isArr {
			t.Fatalf("item[%d] content is an array, want string: %#v", i, item)
		}
		if _, isArr := item["content"].([]map[string]any); isArr {
			t.Fatalf("item[%d] content is an array, want string: %#v", i, item)
		}
	}
}

func TestMessagesToInputEmbedsImagesAsInputImageParts(t *testing.T) {
	c := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", Extra: map[string]any{"vision": true}}).(*client)
	body, _, _ := c.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "what is this", Images: []string{"data:image/png;base64,AAAA", "data:image/jpeg;base64,BBBB"}},
	}})
	items := body["input"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("input items = %d, want 1", len(items))
	}
	parts, ok := items[0]["content"].([]map[string]string)
	if !ok {
		t.Fatalf("user content = %#v, want InputItemList array (image turn)", items[0]["content"])
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3 (text + 2 images)", len(parts))
	}
	if parts[0]["type"] != "input_text" || parts[0]["text"] != "what is this" {
		t.Fatalf("parts[0] = %#v, want input_text", parts[0])
	}
	for i, want := range []string{"data:image/png;base64,AAAA", "data:image/jpeg;base64,BBBB"} {
		if parts[i+1]["type"] != "input_image" || parts[i+1]["image_url"] != want {
			t.Fatalf("parts[%d] = %#v, want input_image %q", i+1, parts[i+1], want)
		}
	}
	// Vision disabled: images are ignored, content stays a string.
	plain := New(Config{Name: "test", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash"}).(*client)
	body2, _, _ := plain.buildRequestBody(provider.Request{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "what is this", Images: []string{"data:image/png;base64,AAAA"}},
	}})
	items2 := body2["input"].([]map[string]any)
	if got, ok := items2[0]["content"].(string); !ok || got != "what is this" {
		t.Fatalf("vision-off user content = %#v, want string", items2[0]["content"])
	}
}
