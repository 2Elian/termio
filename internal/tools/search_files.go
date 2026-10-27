package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"termio/internal/protocol"
)

const (
	defaultSearchResults = 200
	maxSearchResults     = 1000
	defaultSearchDepth   = 16
	maxSearchDepth       = 64
)

// SearchFiles performs filename search (substring or glob on the base name).
// It does not search file contents.
type SearchFiles struct {
	sandbox *Sandbox
}

type searchArgs struct {
	Path          string `json:"path,omitempty"`
	Pattern       string `json:"pattern"`
	MaxResults    int    `json:"max_results,omitempty"`
	IncludeHidden bool   `json:"include_hidden,omitempty"`
	MaxDepth      int    `json:"max_depth,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
}

type searchMatch struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type searchResult struct {
	Matches   []searchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
	Scanned   int           `json:"scanned"`
	Errors    int           `json:"errors"`
	Root      string        `json:"root"`
}

func (t *SearchFiles) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name: "search_files",
		Description: "Search file names under the workspace (or an allowed root). The pattern is a substring, " +
			"or a glob such as *.log matched against the base name. Searches names only, not contents. " +
			"Hidden entries and common dependency directories (.git, node_modules, vendor, .venv, " +
			"__pycache__) are skipped unless include_hidden is true.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"Directory to search; absolute or relative to the workspace. Defaults to the workspace."},
				"pattern":{"type":"string","description":"Filename substring or glob matched against the base name."},
				"max_results":{"type":"integer","minimum":1,"maximum":1000,"description":"Maximum matches to return (default 200)."},
				"include_hidden":{"type":"boolean","description":"Include dot-prefixed entries and common dependency directories."},
				"max_depth":{"type":"integer","minimum":1,"maximum":64,"description":"Maximum directory depth relative to the search root (default 16)."},
				"case_sensitive":{"type":"boolean","description":"Match case-sensitively (default false)."}
			},
			"required":["pattern"],
			"additionalProperties":false
		}`),
	}
}

func (t *SearchFiles) Call(ctx context.Context, raw json.RawMessage, progress ProgressFunc) (protocol.CallToolResult, error) {
	var args searchArgs
	if err := decodeArgs(raw, &args); err != nil {
		return protocol.CallToolResult{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return protocol.CallToolResult{}, &ArgError{Message: "pattern is required"}
	}
	if len(args.Pattern) > 256 {
		return protocol.CallToolResult{}, &ArgError{Message: "pattern is too long (max 256 bytes)"}
	}
	if args.MaxResults < 0 {
		return protocol.CallToolResult{}, &ArgError{Message: "max_results must be positive"}
	}
	if args.MaxDepth < 0 {
		return protocol.CallToolResult{}, &ArgError{Message: "max_depth must be positive"}
	}
	maxResults := args.MaxResults
	if maxResults == 0 {
		maxResults = defaultSearchResults
	}
	if maxResults > maxSearchResults {
		maxResults = maxSearchResults
	}
	maxDepth := args.MaxDepth
	if maxDepth == 0 {
		maxDepth = defaultSearchDepth
	}
	if maxDepth > maxSearchDepth {
		maxDepth = maxSearchDepth
	}

	if args.Path == "" {
		args.Path = t.sandbox.Workspace()
	}
	resolved, err := t.sandbox.Resolve(args.Path)
	if err != nil {
		return protocol.CallToolResult{}, err
	}
	info, err := resolved.Entry.root.Stat(resolved.RelOS)
	if err != nil {
		return protocol.CallToolResult{}, fmt.Errorf("stat %s: %w", resolved.Abs, err)
	}
	if !info.IsDir() {
		return protocol.CallToolResult{}, fmt.Errorf("%s is not a directory", resolved.Abs)
	}

	match, err := newNameMatcher(args.Pattern, args.CaseSensitive)
	if err != nil {
		return protocol.CallToolResult{}, err
	}

	result := searchResult{
		Matches: make([]searchMatch, 0, min(maxResults, 64)),
		Root:    resolved.Abs,
	}

	walkErr := fs.WalkDir(resolved.Entry.root.FS(), resolved.RelSlash, func(p string, d fs.DirEntry, walkErr error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if walkErr != nil {
			result.Errors++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		result.Scanned++
		if progress != nil && result.Scanned%512 == 0 {
			progress(float64(result.Scanned), 0, fmt.Sprintf("scanned %d entries", result.Scanned))
		}

		isStart := p == resolved.RelSlash
		relToStart, err := slashRel(resolved.RelSlash, p)
		if err != nil {
			result.Errors++
			return nil
		}
		depth := 0
		if relToStart != "." {
			depth = strings.Count(relToStart, "/") + 1
		}
		if depth > maxDepth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		name := d.Name()
		if !isStart {
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if !args.IncludeHidden {
				if strings.HasPrefix(name, ".") || (ignoredDir(name) && d.IsDir()) {
					if d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
			}
		}
		if d.IsDir() {
			return nil
		}
		if !match(name) {
			return nil
		}
		fileInfo, err := d.Info()
		if err != nil {
			result.Errors++
			return nil
		}
		if !fileInfo.Mode().IsRegular() {
			return nil
		}
		result.Matches = append(result.Matches, searchMatch{
			Path:    filepath.Join(resolved.Entry.display, filepath.FromSlash(p)),
			Size:    fileInfo.Size(),
			ModTime: fileInfo.ModTime().UTC(),
		})
		if len(result.Matches) >= maxResults {
			result.Truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return protocol.CallToolResult{}, walkErr
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching file(s)", len(result.Matches))
	if result.Truncated {
		b.WriteString(" (truncated)")
	}
	fmt.Fprintf(&b, " under %s; scanned %d entries", result.Root, result.Scanned)
	if result.Errors > 0 {
		fmt.Fprintf(&b, "; skipped %d unreadable entries", result.Errors)
	}
	for _, m := range result.Matches {
		b.WriteString("\n")
		b.WriteString(m.Path)
	}
	return textResult(b.String(), result), nil
}

// slashRel computes target relative to base for slash-separated paths rooted
// at a common fs.FS root. Both paths are already validated by fs.WalkDir.
func slashRel(base, target string) (string, error) {
	base = path.Clean(base)
	target = path.Clean(target)
	if base == "." {
		return target, nil
	}
	if target == base {
		return ".", nil
	}
	if strings.HasPrefix(target, base+"/") {
		return target[len(base)+1:], nil
	}
	return "", fmt.Errorf("%q is not under %q", target, base)
}

func ignoredDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "vendor", ".venv", "__pycache__":
		return true
	default:
		return false
	}
}

func newNameMatcher(pattern string, caseSensitive bool) (func(string) bool, error) {
	if strings.ContainsAny(pattern, "*?[\\") {
		pat := pattern
		if !caseSensitive {
			pat = strings.ToLower(pat)
		}
		if _, err := path.Match(pat, ""); err != nil {
			return nil, &ArgError{Message: fmt.Sprintf("invalid glob pattern %q: %v", pattern, err)}
		}
		return func(name string) bool {
			if !caseSensitive {
				name = strings.ToLower(name)
			}
			ok, _ := path.Match(pat, name)
			return ok
		}, nil
	}
	if caseSensitive {
		return func(name string) bool { return strings.Contains(name, pattern) }, nil
	}
	lower := strings.ToLower(pattern)
	return func(name string) bool { return strings.Contains(strings.ToLower(name), lower) }, nil
}
