package evidence

import (
	"path/filepath"
	"testing"
)

func TestBashScratchDeliveryScopeIsNarrow(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	script := filepath.Join(scratch, "probe.py")
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "scratch script", command: "python " + script, want: true},
		{name: "stderr merge", command: "python " + script + " 2>&1", want: true},
		{name: "workspace script", command: "python probe.py", want: false},
		{name: "redirect writes workspace", command: "python " + script + " > result.txt", want: false},
		{name: "extra argument may be output", command: "python " + script + " result.txt", want: false},
		{name: "inline code", command: "python -c 'print(1)'", want: false},
		{name: "unknown runner", command: "custom-runner " + script, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BashScratchDeliveryScope(tt.command, workspace, []string{scratch}); got != tt.want {
				t.Fatalf("BashScratchDeliveryScope(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
