package evidence

import (
	"os"
	"path/filepath"
	"strings"
)

// WriteScope is where a write lands relative to the project, not how risky
// the path name looks. PathClass still owns auth/schema/docs risk.
type WriteScope uint8

const (
	WriteScopeWorkspace WriteScope = iota
	WriteScopeScratch
	WriteScopeOutside
)

func (s WriteScope) String() string {
	switch s {
	case WriteScopeScratch:
		return "scratch"
	case WriteScopeOutside:
		return "outside"
	default:
		return "workspace"
	}
}

// ClassifyWriteScope reports whether path is inside the workspace, a scratch
// root (session temp or OS temp), or somewhere else. Relative paths without a
// workspace root stay workspace so existing receipts keep their meaning.
func ClassifyWriteScope(path, workspaceRoot string, scratchRoots []string) WriteScope {
	path = strings.TrimSpace(path)
	if path == "" {
		return WriteScopeWorkspace
	}
	abs := scopeAbs(path, workspaceRoot)
	if workspaceRoot != "" && lexicallyInside(workspaceRoot, abs) {
		return WriteScopeWorkspace
	}
	if isSessionTempPath(abs) {
		return WriteScopeScratch
	}
	if !filepath.IsAbs(path) && strings.TrimSpace(workspaceRoot) == "" {
		return WriteScopeWorkspace
	}
	for _, root := range scratchRootList(scratchRoots) {
		if lexicallyInside(root, abs) {
			return WriteScopeScratch
		}
	}
	if filepath.IsAbs(abs) {
		return WriteScopeOutside
	}
	return WriteScopeWorkspace
}

// DefaultScratchRoots is the OS temporary directory plus the usual Unix
// aliases. Session-private dirs should be passed in explicitly when known.
func DefaultScratchRoots() []string {
	// Only the public temp aliases. Do not add os.TempDir() when it is a
	// per-user harness like macOS /var/folders: tests and checkouts live there
	// and must stay classifiable as workspace/outside until a workspace root
	// is supplied.
	roots := []string{"/tmp", "/private/tmp"}
	if tmp := os.TempDir(); tmp != "" {
		switch filepath.Clean(tmp) {
		case "/tmp", "/private/tmp", `C:\Windows\Temp`, `C:\Temp`:
			roots = append(roots, tmp)
		}
	}
	return uniqueCleanRoots(roots)
}

func scratchRootList(extra []string) []string {
	return uniqueCleanRoots(append(DefaultScratchRoots(), extra...))
}

func isSessionTempPath(path string) bool {
	return strings.Contains(filepath.ToSlash(path), "/reasonix-session-tmp-")
}

func uniqueCleanRoots(roots []string) []string {
	seen := make(map[string]bool, len(roots))
	var out []string
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		cleaned := filepath.Clean(root)
		if resolved, err := filepath.EvalSymlinks(cleaned); err == nil && resolved != "" {
			cleaned = resolved
		}
		key := strings.ToLower(cleaned)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cleaned)
	}
	return out
}

func scopeAbs(path, workspaceRoot string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(workspaceRoot, path))
}

func lexicallyInside(root, target string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	target = filepath.Clean(strings.TrimSpace(target))
	if root == "" || target == "" {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil && resolved != "" {
		target = resolved
	} else {
		target = evalExistingPrefix(target)
	}
	if filepath.VolumeName(root) != filepath.VolumeName(target) {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func evalExistingPrefix(path string) string {
	cur := filepath.Clean(path)
	tail := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil && resolved != "" {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		dir := filepath.Dir(cur)
		if dir == cur {
			return filepath.Clean(path)
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = dir
	}
}
