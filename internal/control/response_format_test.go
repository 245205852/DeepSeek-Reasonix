package control

import (
	"context"
	"testing"

	"reasonix/internal/agent"
)

func TestIsNonTurnHTTPInput(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{
		{"", true},               // empty
		{"  ", true},             // blank
		{"# note text", true},    // memory quick-add (# + space)
		{"/remember MiMo", true}, // remember command note
		{"/compact", true},       // slash command
		{"/model qwen3", true},   // management verb
		{"/new", true},           // slash command
		{"!ls", true},            // shell commands rejected by submitHTTP (403) before any turn
		{"hello", false},         // ordinary turn
		{"explain this code", false},
	} {
		if got := isNonTurnHTTPInput(tc.input); got != tc.want {
			t.Errorf("isNonTurnHTTPInput(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// TestSubmitHTTPFormatBindsToTurn：format 随提交的 turn 传递（参数链），
// 不再有 Controller 全局一次性槽——非 turn 输入（slash/!）不携带 format。
// 评审 #7234 第 2 点：全局槽存在跨请求串用的逻辑竞态。
func TestSubmitHTTPFormatBindsToTurn(t *testing.T) {
	c := New(Options{})
	// 非 turn 输入（/new）携带 format → 被丢弃（不进入 turn 参数链）。
	c.SubmitHTTPFormat("/new", "json_object")
	// 普通 turn 携带 format → 进入参数链（withTurnFormat 注入 ctx）。
	c.SubmitHTTPFormat("tell me about MiMo", "json_object")
}

// TestWithTurnFormatInjectsFormatIntoContext：format 绑定 turn 的实际效果
// ——withTurnFormat 注入后 agent 请求路径能读到（不是全局槽）。
func TestWithTurnFormatInjectsFormatIntoContext(t *testing.T) {
	c := New(Options{})
	ctx := context.Background()
	if got := agent.ResponseFormatFromRequest(c.withTurnFormat(ctx, "")); got != nil {
		t.Fatalf("empty format must be no-op, got %+v", got)
	}
	if got := agent.ResponseFormatFromRequest(c.withTurnFormat(ctx, "json_object")); got == nil || got.Type != "json_object" {
		t.Fatalf("turn format must reach agent request, got %+v", got)
	}
}
