package sessioninbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FreezeRefs inspects @-style path tokens already extracted by the caller and
// freezes clean git content by commit identity, or snapshots dirty/external
// bytes into the envelope. It does not expand an entire repository into the
// prompt.
func FreezeRefs(ctx context.Context, workspace string, paths []string) ([]RefSnapshot, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	workspace = strings.TrimSpace(workspace)
	out := make([]RefSnapshot, 0, len(paths))
	seen := map[string]struct{}{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		snap, err := freezeOne(ctx, workspace, p)
		if err != nil {
			// Keep a blocked-style frozen marker; admission will surface block.
			out = append(out, RefSnapshot{
				Kind:        "frozen",
				Path:        p,
				DisplayPath: p,
				Content:     []byte(fmt.Sprintf("/* ref freeze failed: %v */", err)),
			})
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

func freezeOne(ctx context.Context, workspace, relOrAbs string) (RefSnapshot, error) {
	path, display, err := resolveScopedPath(workspace, relOrAbs)
	if err != nil {
		return RefSnapshot{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return RefSnapshot{}, err
	}
	_ = display
	if info.IsDir() {
		return RefSnapshot{
			Kind:        "frozen",
			Path:        display,
			DisplayPath: display,
			Content:     []byte("(directory reference — expanded at execution)"),
		}, nil
	}
	// Prefer clean git commit identity when the file is tracked and clean.
	if commit, repo, ok := cleanGitIdentity(ctx, path); ok {
		return RefSnapshot{
			Kind:         "clean_git",
			Path:         display,
			DisplayPath:  display,
			RepoIdentity: repo,
			Commit:       commit,
		}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RefSnapshot{}, err
	}
	const maxFreeze = DefaultMaxItemBytes
	truncated := false
	if len(data) > maxFreeze {
		data = data[:maxFreeze]
		truncated = true
	}
	sum := sha256.Sum256(data)
	return RefSnapshot{
		Kind:        "frozen",
		Path:        display,
		DisplayPath: display,
		Content:     data,
		ContentSHA:  hex.EncodeToString(sum[:]),
		Truncated:   truncated,
	}, nil
}

// resolveScopedPath confines reads to workspace (or session attachments under
// workspace). Absolute paths outside the workspace are rejected to prevent
// host-file exfiltration via inbox freeze/HTTP get.
func resolveScopedPath(workspace, relOrAbs string) (abs, display string, err error) {
	workspace = strings.TrimSpace(workspace)
	relOrAbs = strings.TrimSpace(relOrAbs)
	if relOrAbs == "" {
		return "", "", fmt.Errorf("empty path")
	}
	if workspace == "" {
		return "", "", fmt.Errorf("path requires workspace root")
	}
	cleanWS, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return "", "", err
	}
	realWS, err := filepath.EvalSymlinks(cleanWS)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace root: %w", err)
	}
	candidate := filepath.Clean(relOrAbs)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cleanWS, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", "", err
	}
	// Resolve before reading and return the resolved path. A workspace-local
	// symlink that targets outside the workspace is therefore rejected, and a
	// later read does not traverse the user-supplied symlink again.
	realPath, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", fmt.Errorf("resolve reference path: %w", err)
	}
	rel, err := filepath.Rel(realWS, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("path escapes workspace: %s", relOrAbs)
	}
	return realPath, filepath.ToSlash(rel), nil
}

// ApplyFrozenRefs rebuilds submit text so frozen reference bodies are what the
// model sees at execution time (not a later dirty workspace).
func ApplyFrozenRefs(submit string, bodies map[string]string) string {
	if len(bodies) == 0 {
		return submit
	}
	var b strings.Builder
	b.WriteString(submit)
	b.WriteString("\n\n<!-- frozen inbox references (enqueue-time snapshot) -->\n")
	paths := make([]string, 0, len(bodies))
	for path := range bodies {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		content := bodies[path]
		fmt.Fprintf(&b, "\n### @%s\n```\n%s\n```\n", path, content)
	}
	return b.String()
}

func cleanGitIdentity(ctx context.Context, path string) (commit, repo string, ok bool) {
	dir := filepath.Dir(path)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// Must be inside a git work tree.
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", "", false
	}
	// Untracked or dirty → not clean.
	status, err := runGit(ctx, dir, "status", "--porcelain", "--", path)
	if err != nil {
		return "", "", false
	}
	if strings.TrimSpace(status) != "" {
		return "", "", false
	}
	// Tracked?
	_, err = runGit(ctx, dir, "ls-files", "--error-unmatch", "--", path)
	if err != nil {
		return "", "", false
	}
	head, err := runGit(ctx, dir, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return "", "", false
	}
	return head, top, true
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// MaterializeRefs expands frozen refs for execution. Clean git refs are read
// from the recorded commit; frozen blobs use stored content. Failures return
// a block reason without deleting the item.
func MaterializeRefs(ctx context.Context, workspace string, refs []RefSnapshot) (block string, bodies map[string]string, err error) {
	bodies = make(map[string]string, len(refs))
	for _, r := range refs {
		switch r.Kind {
		case "clean_git":
			content, e := gitShow(ctx, workspace, r.Commit, r.Path)
			if e != nil {
				return fmt.Sprintf("commit %s missing or unreadable for %s: %v", r.Commit, r.Path, e), bodies, nil
			}
			bodies[r.Path] = content
		case "frozen", "external", "attachment", "mcp":
			if r.ContentSHA != "" {
				sum := sha256.Sum256(r.Content)
				if hex.EncodeToString(sum[:]) != r.ContentSHA {
					return fmt.Sprintf("checksum mismatch for frozen ref %s", r.Path), bodies, nil
				}
			}
			bodies[r.Path] = string(r.Content)
		default:
			bodies[r.Path] = string(r.Content)
		}
	}
	return "", bodies, nil
}

func gitShow(ctx context.Context, workspace, commit, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// path may be workspace-relative.
	out, err := runGit(ctx, workspace, "show", commit+":"+path)
	if err != nil {
		// Try absolute-to-relative against toplevel.
		top, e2 := runGit(ctx, workspace, "rev-parse", "--show-toplevel")
		if e2 == nil {
			rel := path
			if filepath.IsAbs(path) {
				if r, e3 := filepath.Rel(top, path); e3 == nil {
					rel = r
				}
			}
			out, err = runGit(ctx, top, "show", commit+":"+rel)
		}
	}
	return out, err
}
