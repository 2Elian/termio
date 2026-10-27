package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// ErrOutsideRoots is returned when a requested path is not under the workspace
// or an extra allowed root. Errors from os.Root are returned for symlink
// escapes discovered at open time.
var ErrOutsideRoots = errors.New("path is outside the allowed roots")

// Sandbox restricts file access to a workspace plus optional extra roots. It
// uses os.Root so symlink and junction escapes are rejected by the operating
// system at open time, not merely by string comparison.
type Sandbox struct {
	workspace string
	all       []string // workspace first, then extras (deduplicated, display order)
	roots     []*rootEntry
}

type rootEntry struct {
	display string
	root    *os.Root
}

// Resolved is a path mapped to one sandbox root.
type Resolved struct {
	Entry    *rootEntry
	RelSlash string // slash-separated path for fs.FS
	RelOS    string // OS-separator path for os.Root.Open/Stat
	Abs      string // cleaned absolute path as requested
}

// NewSandbox opens the workspace and extra roots. All paths must be existing
// directories; callers normally pass config.NormalizeWorkspace output.
func NewSandbox(workspace string, extraRoots []string) (*Sandbox, error) {
	ws, err := normalizeSandboxDir(workspace, "workspace")
	if err != nil {
		return nil, err
	}
	all := []string{ws}
	seen := map[string]bool{pathKey(ws): true}
	for _, raw := range extraRoots {
		normalized, err := normalizeSandboxDir(raw, "allowed root")
		if err != nil {
			return nil, err
		}
		key := pathKey(normalized)
		if seen[key] {
			continue
		}
		seen[key] = true
		all = append(all, normalized)
	}

	sb := &Sandbox{workspace: ws, all: all}
	for _, path := range all {
		root, err := os.OpenRoot(path)
		if err != nil {
			sb.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		sb.roots = append(sb.roots, &rootEntry{display: path, root: root})
	}
	// Longest path first so nested roots win over their parents.
	sort.SliceStable(sb.roots, func(i, j int) bool {
		return len(sb.roots[i].display) > len(sb.roots[j].display)
	})
	return sb, nil
}

// Close releases all root handles.
func (s *Sandbox) Close() error {
	var errs []error
	for _, entry := range s.roots {
		if err := entry.root.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Keep the slices intact: an in-flight tool may still call Resolve while
	// shutdown is completing. Closed roots make subsequent opens fail
	// without racing on the slices themselves.
	return errors.Join(errs...)
}

// Workspace returns the canonical workspace path.
func (s *Sandbox) Workspace() string { return s.workspace }

// Roots returns the effective roots, workspace first, then deduplicated extras.
func (s *Sandbox) Roots() []string {
	out := make([]string, len(s.all))
	copy(out, s.all)
	return out
}

// Resolve maps a request path to a root. An empty path or a relative path is
// resolved against the workspace. Relative paths may not escape the workspace
// with "..". Absolute paths may use the workspace or an extra root. Symlink
// escapes inside a root are detected later by os.Root when the path is opened
// or walked.
func (s *Sandbox) Resolve(requestPath string) (*Resolved, error) {
	requested := requestPath
	if requested == "" {
		requested = s.workspace
	}
	if !filepath.IsAbs(requested) {
		clean := filepath.Clean(requested)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, &ArgError{Message: "relative path escapes the workspace"}
		}
		requested = filepath.Join(s.workspace, clean)
	}
	if err := validateAbsolutePath(requested); err != nil {
		return nil, err
	}
	// Canonicalize the request as well as the roots. This maps Windows 8.3
	// short names and symlinked request paths onto the canonical root before
	// containment is checked. os.Root still re-checks at open time.
	canonical, err := resolveExisting(requested)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", requested, err)
	}
	for _, entry := range s.roots {
		rel, err := filepath.Rel(entry.display, canonical)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		relSlash := "."
		if rel != "." {
			relSlash = filepath.ToSlash(rel)
		}
		return &Resolved{
			Entry:    entry,
			RelSlash: relSlash,
			RelOS:    filepath.FromSlash(relSlash),
			Abs:      canonical,
		}, nil
	}
	return nil, ErrOutsideRoots
}

func normalizeSandboxDir(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s path is empty", label)
	}
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", label, path, err)
	}
	if runtime.GOOS == "windows" && (strings.HasPrefix(abs, `\\`) || strings.HasPrefix(abs, "//")) {
		return "", fmt.Errorf("%s %q: UNC and device paths are not supported", label, path)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", label, path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", label, path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", label, path)
	}
	return filepath.Clean(resolved), nil
}

func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// resolveExisting canonicalizes p by resolving symlinks in its longest existing
// ancestor and re-appending the missing suffix. Read-only tools use it so a
// not-yet-existing path still maps to a canonical location.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	cur := p
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func validateAbsolutePath(p string) error {
	if p == "" {
		return &ArgError{Message: "path is required"}
	}
	if strings.ContainsRune(p, 0) {
		return &ArgError{Message: "path contains a NUL byte"}
	}
	if len(p) > 4096 {
		return &ArgError{Message: "path is too long"}
	}
	if !filepath.IsAbs(p) {
		return &ArgError{Message: "path must be absolute or relative to the workspace"}
	}
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return &ArgError{Message: "UNC and device paths are not supported"}
		}
		vol := filepath.VolumeName(p)
		if len(vol) != 2 || vol[1] != ':' {
			return &ArgError{Message: "path must be a local drive path"}
		}
		if strings.Contains(p[len(vol):], ":") {
			return &ArgError{Message: "alternate data stream paths are not supported"}
		}
	}
	return nil
}

func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
