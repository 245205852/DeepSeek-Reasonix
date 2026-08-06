package shellrun

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"reasonix/internal/proc"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

func TestDescriptorFromShell(t *testing.T) {
	tests := []struct {
		name        string
		sh          sandbox.Shell
		wantShell   string
		wantVersion string
		wantAndAnd  bool
	}{
		{
			name:       "posix bash",
			sh:         sandbox.Shell{Kind: sandbox.ShellBash, Path: "/bin/bash"},
			wantShell:  tool.ShellNameBash,
			wantAndAnd: true,
		},
		{
			name:       "git bash path",
			sh:         sandbox.Shell{Kind: sandbox.ShellBash, Path: `C:\Program Files\Git\bin\bash.exe`},
			wantShell:  tool.ShellNameGitBash,
			wantAndAnd: true,
		},
		{
			name:        "windows powershell 5.1",
			sh:          sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`},
			wantShell:   tool.ShellNamePowerShell,
			wantVersion: tool.ShellVersionPS51,
			wantAndAnd:  false,
		},
		{
			name:        "pwsh 7+",
			sh:          sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: `C:\Program Files\PowerShell\7\pwsh.exe`},
			wantShell:   tool.ShellNamePwsh,
			wantVersion: tool.ShellVersionPS7,
			wantAndAnd:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DescriptorFromShell(tt.sh)
			if got.Shell != tt.wantShell {
				t.Fatalf("Shell = %q, want %q", got.Shell, tt.wantShell)
			}
			if got.ShellVersion != tt.wantVersion {
				t.Fatalf("ShellVersion = %q, want %q", got.ShellVersion, tt.wantVersion)
			}
			if got.SupportsAndAnd != tt.wantAndAnd {
				t.Fatalf("SupportsAndAnd = %v, want %v", got.SupportsAndAnd, tt.wantAndAnd)
			}
			if got.Kind != "shell" {
				t.Fatalf("Kind = %q", got.Kind)
			}
			if got.Platform == "" {
				t.Fatal("Platform empty")
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	if got := DisplayName(DescriptorFromShell(sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: "powershell"})); got != "Windows PowerShell" {
		t.Fatalf("got %q", got)
	}
	if got := DisplayName(DescriptorFromShell(sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: "pwsh"})); got != "PowerShell 7+" {
		t.Fatalf("got %q", got)
	}
	if got := DisplayName(DescriptorFromShell(sandbox.Shell{Kind: sandbox.ShellBash, Path: `C:\Program Files\Git\bin\bash.exe`})); got != "Git Bash" {
		t.Fatalf("got %q", got)
	}
}

func TestRunForegroundSuccess(t *testing.T) {
	argv, sh := shellArgv(t, "printf 'ok\\n'")
	res := RunForeground(context.Background(), Request{
		Argv:      argv,
		ShellKind: sh.Kind.String(),
		ShellPath: sh.Path,
		Track:     true,
	})
	if res.Err != nil {
		t.Fatalf("err = %v", res.Err)
	}
	if res.State != tool.ShellStateCompleted {
		t.Fatalf("state = %q", res.State)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("exitCode = %v", res.ExitCode)
	}
	if !strings.Contains(res.Combined, "ok") {
		t.Fatalf("combined = %q", res.Combined)
	}
}

func TestRunForegroundNonZeroExit(t *testing.T) {
	argv, sh := shellArgv(t, "exit 7")
	res := RunForeground(context.Background(), Request{
		Argv:      argv,
		ShellKind: sh.Kind.String(),
		ShellPath: sh.Path,
		Track:     true,
	})
	if res.Err == nil {
		t.Fatal("expected error")
	}
	if res.State != tool.ShellStateFailed || res.FailurePhase != tool.ShellPhaseExecution {
		t.Fatalf("state/phase = %s/%s", res.State, res.FailurePhase)
	}
	if res.ExitCode == nil || *res.ExitCode == 0 {
		t.Fatalf("exitCode = %v", res.ExitCode)
	}
}

func TestRunForegroundTimeout(t *testing.T) {
	cmd := "sleep 5"
	sh := sandbox.ResolveShell("auto", "", nil)
	if sh.Kind == sandbox.ShellPowerShell {
		cmd = "Start-Sleep -Seconds 5"
	}
	argv, _ := shellArgv(t, cmd)
	res := RunForeground(context.Background(), Request{
		Argv:      argv,
		Timeout:   200 * time.Millisecond,
		ShellKind: sh.Kind.String(),
		ShellPath: sh.Path,
		Track:     true,
	})
	if res.State != tool.ShellStateTimedOut || res.FailurePhase != tool.ShellPhaseTimeout {
		t.Fatalf("state/phase = %s/%s err=%v", res.State, res.FailurePhase, res.Err)
	}
}

func TestRunForegroundLaunchFailure(t *testing.T) {
	res := RunForeground(context.Background(), Request{
		Argv:  []string{"/nonexistent/reasonix-shell-binary-xyz", "-c", "echo hi"},
		Track: false,
		Run: func(ctx context.Context, cmd *exec.Cmd, opts proc.RunOptions) (*proc.TrackedCommand, error) {
			return nil, errors.New("exec: no such file")
		},
	})
	if res.State != tool.ShellStateFailed || res.FailurePhase != tool.ShellPhaseLaunch {
		t.Fatalf("state/phase = %s/%s", res.State, res.FailurePhase)
	}
	if res.ExitCode != nil {
		t.Fatalf("exitCode should be nil for launch failure, got %v", *res.ExitCode)
	}
}

func TestRunForegroundStderrTailBounded(t *testing.T) {
	payload := strings.Repeat("中文", 3000)
	// Keep the command under typical argv length limits.
	if len(payload) > 4000 {
		payload = payload[:4000]
	}
	sh := sandbox.ResolveShell("auto", "", nil)
	var command string
	if sh.Kind == sandbox.ShellPowerShell {
		command = `[Console]::Error.Write('` + strings.ReplaceAll(payload, "'", "''") + `')`
	} else {
		command = "printf '%s' '" + strings.ReplaceAll(payload, "'", `'\"'\"'`) + "' 1>&2"
	}
	argv := shellArgvWith(sh, command)
	res := RunForeground(context.Background(), Request{
		Argv:      argv,
		ShellKind: sh.Kind.String(),
		ShellPath: sh.Path,
		Track:     true,
	})
	if len(res.StderrTail) > tool.StderrTailMaxBytes {
		t.Fatalf("stderr tail %d > %d", len(res.StderrTail), tool.StderrTailMaxBytes)
	}
	if !strings.Contains(res.Combined, "中文") && !strings.Contains(res.StderrTail, "中文") {
		t.Fatalf("UTF-8 Chinese lost: combined=%q tail=%q", trim(res.Combined, 80), trim(res.StderrTail, 80))
	}
}

func shellArgv(t *testing.T, command string) ([]string, sandbox.Shell) {
	t.Helper()
	sh := sandbox.ResolveShell("auto", "", nil)
	return shellArgvWith(sh, command), sh
}

func shellArgvWith(sh sandbox.Shell, command string) []string {
	path := sh.Path
	if path == "" {
		path = sh.Kind.String()
	}
	if sh.Kind == sandbox.ShellPowerShell {
		return []string{path, "-NoProfile", "-NonInteractive", "-Command", sandbox.PowerShellUTF8Script(command)}
	}
	return []string{path, "-c", command}
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
