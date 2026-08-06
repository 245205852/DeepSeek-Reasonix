package evidence

import (
	"encoding/json"
	"testing"
)

func TestBashToolCallUsesNonTerminalInlineInterpreter(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{command: `python3 -c 'open("x","w").write("y")' ; node verify_frontend_logic.js`, want: true},
		{command: `node -e 'console.log(1)' && go test ./...`, want: true},
		{command: `python3 -c 'print(1)'`, want: false},
		{command: `go test ./...`, want: false},
		{command: `node --check app.js`, want: false},
	}
	for _, tt := range tests {
		args, err := json.Marshal(map[string]string{"command": tt.command})
		if err != nil {
			t.Fatal(err)
		}
		if got := BashToolCallUsesNonTerminalInlineInterpreter(args); got != tt.want {
			t.Errorf("%q => %v, want %v", tt.command, got, tt.want)
		}
	}
}

func TestOrdinaryModeShellContractClassifiers(t *testing.T) {
	// Ordinary-mode blockers re-use the same classifiers as Delivery for
	// mixed / mask / non-terminal-inline shapes.
	cases := []struct {
		command string
		mixed   bool
		mask    bool
		inline  bool
	}{
		// Arbitrary node scripts are not host-recognized verifiers; the
		// non-terminal inline interpreter rule still blocks this shape.
		{command: `python3 -c 'open("/tmp/x","w").write("x")' ; node verify_frontend_logic.js`, inline: true},
		{command: `go generate ./... && go test ./...`, mixed: true},
		// Masked exit is also mixed (echo is not a verifier); agent checks mask first.
		{command: `go test ./...; echo $?`, mixed: true, mask: true},
		{command: `python3 -c 'print(1)'`},
		{command: `go test ./...`},
		{command: `tail -n +1 file | node --check -`},
	}
	for _, tt := range cases {
		args, _ := json.Marshal(map[string]string{"command": tt.command})
		if got := BashToolCallMixesMutationAndVerification(args); got != tt.mixed {
			t.Errorf("mixed(%q)=%v want %v", tt.command, got, tt.mixed)
		}
		if got := BashToolCallMasksVerificationExit(args); got != tt.mask {
			t.Errorf("mask(%q)=%v want %v", tt.command, got, tt.mask)
		}
		if got := BashToolCallUsesNonTerminalInlineInterpreter(args); got != tt.inline {
			t.Errorf("inlineNT(%q)=%v want %v", tt.command, got, tt.inline)
		}
	}
}
