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
	if workspaceRoot != "" && pathInside(workspaceRoot, abs) {
		return WriteScopeWorkspace
	}
	if !filepath.IsAbs(path) && strings.TrimSpace(workspaceRoot) == "" {
		return WriteScopeWorkspace
	}
	for _, root := range scratchRootList(scratchRoots) {
		if pathInside(root, abs) {
			return WriteScopeScratch
		}
	}
	if filepath.IsAbs(abs) {
		return WriteScopeOutside
	}
	return WriteScopeWorkspace
}

// DefaultScratchRoots includes the OS temp directory and Unix public aliases.
// A supplied workspace root always wins for checkouts located under temp.
func DefaultScratchRoots() []string {
	roots := []string{os.TempDir()}
	if filepath.Separator == '/' {
		roots = append(roots, "/tmp", "/private/tmp")
	}
	return uniqueCleanRoots(roots)
}

func scratchRootList(extra []string) []string {
	return uniqueCleanRoots(append(DefaultScratchRoots(), extra...))
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

func pathInside(root, target string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	target = filepath.Clean(strings.TrimSpace(target))
	if root == "" || target == "" {
		return false
	}
	if !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(target)) {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
