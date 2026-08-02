package control

import "testing"

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

func TestSubmitHTTPFormatDoesNotLeakOnNonTurn(t *testing.T) {
	c := New(Options{})
	// A slash command with a format must NOT leave the slot set.
	c.SubmitHTTPFormat("/new", "json_object")
	c.mu.Lock()
	got := c.pendingResponseFormat
	c.mu.Unlock()
	if got != "" {
		t.Fatalf("pendingResponseFormat leaked after slash command: %q", got)
	}
	// An ordinary turn with a format sets the slot (consumed by the loop).
	c.SubmitHTTPFormat("tell me about MiMo", "json_object")
	c.mu.Lock()
	got = c.pendingResponseFormat
	c.mu.Unlock()
	if got != "json_object" {
		t.Fatalf("pendingResponseFormat = %q, want json_object", got)
	}
	// takePendingResponseFormat consumes it one-shot.
	if f := c.takePendingResponseFormat(); f != "json_object" {
		t.Fatalf("takePendingResponseFormat = %q, want json_object", f)
	}
	if f := c.takePendingResponseFormat(); f != "" {
		t.Fatalf("second take must be empty, got %q", f)
	}
}
