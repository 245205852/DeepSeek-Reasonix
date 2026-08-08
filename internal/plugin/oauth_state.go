package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
)

func mcpOAuthStatePath(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	return filepath.Join(stateDir, mcpOAuthStateFile)
}

func acquireMCPOAuthStateLock(ctx context.Context, stateDir string) (func(), error) {
	path := mcpOAuthStatePath(stateDir)
	if path == "" {
		return nil, fmt.Errorf("private state directory is unavailable")
	}
	return filelock.Acquire(ctx, path+".lock")
}

func loadMCPOAuthState(stateDir string) (mcpOAuthState, error) {
	path := mcpOAuthStatePath(stateDir)
	if path == "" {
		return mcpOAuthState{}, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mcpOAuthState{}, nil
		}
		return mcpOAuthState{}, fmt.Errorf("read MCP OAuth state: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return mcpOAuthState{}, fmt.Errorf("read MCP OAuth state: refusing non-regular file")
	}
	if info.Size() > maxOAuthBody {
		return mcpOAuthState{}, fmt.Errorf("read MCP OAuth state: file is too large")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return mcpOAuthState{}, fmt.Errorf("read MCP OAuth state: %w", err)
	}
	var state mcpOAuthState
	if err := json.Unmarshal(b, &state); err != nil {
		return mcpOAuthState{}, fmt.Errorf("decode MCP OAuth state: %w", err)
	}
	if state.Version != 1 {
		return mcpOAuthState{}, fmt.Errorf("decode MCP OAuth state: unsupported version %d", state.Version)
	}
	return state, nil
}

func saveMCPOAuthState(stateDir string, state mcpOAuthState) error {
	path := mcpOAuthStatePath(stateDir)
	if path == "" {
		return fmt.Errorf("save MCP OAuth state: private state directory is unavailable")
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("save MCP OAuth state: refusing non-regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("save MCP OAuth state: %w", err)
	}
	state.Version = 1
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode MCP OAuth state: %w", err)
	}
	if err := fileutil.AtomicWriteFileStrict(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("save MCP OAuth state: %w", err)
	}
	return nil
}
