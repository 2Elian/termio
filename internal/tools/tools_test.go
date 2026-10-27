package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"termio/internal/protocol"
)

func writeSearchTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"a.txt",
		"b.log",
		"README.MD",
		"sub/c.txt",
		".hidden/d.txt",
		"node_modules/e.txt",
		"vendor/f.txt",
	}
	for _, name := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func callSearch(t *testing.T, sb *Sandbox, args map[string]any) (protocol.CallToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	tool := &SearchFiles{sandbox: sb}
	return tool.Call(context.Background(), raw, nil)
}

func TestSearchGlobDefaultSkipsHiddenAndIgnored(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	got := matchPaths(res)
	base := sb.Roots()[0]
	want := []string{
		filepath.Join(base, "a.txt"),
		filepath.Join(base, "sub", "c.txt"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestSearchIncludeHidden(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.txt", "include_hidden": true})
	if err != nil {
		t.Fatal(err)
	}
	got := matchPaths(res)
	base := sb.Roots()[0]
	want := []string{
		filepath.Join(base, ".hidden", "d.txt"),
		filepath.Join(base, "a.txt"),
		filepath.Join(base, "node_modules", "e.txt"),
		filepath.Join(base, "sub", "c.txt"),
		filepath.Join(base, "vendor", "f.txt"),
	}
	if !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestSearchMaxDepth(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.txt", "max_depth": 1})
	if err != nil {
		t.Fatal(err)
	}
	got := matchPaths(res)
	want := []string{filepath.Join(sb.Roots()[0], "a.txt")}
	if !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestSearchMaxResultsTruncates(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.txt", "max_results": 1})
	if err != nil {
		t.Fatal(err)
	}
	sr := res.StructuredContent.(searchResult)
	if !sr.Truncated || len(sr.Matches) != 1 {
		t.Fatalf("result = %+v", sr)
	}
}

func TestSearchCaseInsensitiveByDefault(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "readme"})
	if err != nil {
		t.Fatal(err)
	}
	got := matchPaths(res)
	want := []string{filepath.Join(sb.Roots()[0], "README.MD")}
	if !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestSearchCaseSensitive(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.TXT", "case_sensitive": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := matchPaths(res); len(got) != 0 {
		t.Fatalf("paths = %v, want none", got)
	}
}

func TestSearchInvalidPattern(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	_, err := callSearch(t, sb, map[string]any{"pattern": "["})
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestSearchPatternTooLong(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	_, err := callSearch(t, sb, map[string]any{"pattern": strings.Repeat("a", 257)})
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestSearchOutsideRoot(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	outside := t.TempDir()
	_, err := callSearch(t, sb, map[string]any{"pattern": "*", "path": outside})
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("err = %v, want ErrOutsideRoots", err)
	}
}

func TestSearchRelativePath(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	res, err := callSearch(t, sb, map[string]any{"pattern": "*.txt", "path": "sub"})
	if err != nil {
		t.Fatal(err)
	}
	got := matchPaths(res)
	want := []string{filepath.Join(sb.Workspace(), "sub", "c.txt")}
	if !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestSearchRelativeEscape(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	_, err := callSearch(t, sb, map[string]any{"pattern": "*", "path": "../outside"})
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestSearchPathNotDirectory(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	_, err := callSearch(t, sb, map[string]any{"pattern": "*", "path": filepath.Join(root, "a.txt")})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSearchCanceled(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]any{"pattern": "*"})
	tool := &SearchFiles{sandbox: sb}
	_, err := tool.Call(ctx, raw, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSearchRejectsUnknownArgument(t *testing.T) {
	root := writeSearchTree(t)
	sb := newTestSandbox(t, root)
	_, err := callSearch(t, sb, map[string]any{"pattern": "*", "nope": true})
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestReadFileText(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(root, "a.txt")) + `}`)
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(readFileResult)
	if got.Encoding != "utf-8" || got.IsBinary || got.Truncated || got.BytesReturned != 5 {
		t.Fatalf("result = %+v", got)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello" {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestReadFileTruncated(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(root, "a.txt")) + `,"max_bytes":4}`)
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(readFileResult)
	if !got.Truncated || got.BytesReturned != 4 || got.Size != 10 {
		t.Fatalf("result = %+v", got)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "0123" {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestReadFileBinary(t *testing.T) {
	root := t.TempDir()
	data := []byte{0xff, 0xfe, 0x00, 0x01}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(root, "a.bin")) + `}`)
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(readFileResult)
	if !got.IsBinary || got.Encoding != "base64" {
		t.Fatalf("result = %+v", got)
	}
	if len(res.Content) != 1 || res.Content[0].Text != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestReadFileNULIsBinary(t *testing.T) {
	root := t.TempDir()
	data := []byte("abc\x00def")
	if err := os.WriteFile(filepath.Join(root, "a.dat"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(root, "a.dat")) + `}`)
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(readFileResult)
	if !got.IsBinary || got.Encoding != "base64" {
		t.Fatalf("result = %+v", got)
	}
	if res.Content[0].Text != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("content = %q", res.Content[0].Text)
	}
}

func TestReadFileUTF8Boundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aé"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(root, "a.txt")) + `,"max_bytes":2}`)
	res, err := tool.Call(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := res.StructuredContent.(readFileResult)
	if got.IsBinary || !got.Truncated {
		t.Fatalf("result = %+v", got)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "a" {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestReadFileRelativePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	res, err := tool.Call(context.Background(), json.RawMessage(`{"path":"a.txt"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello" {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestReadFileRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(root) + `}`)
	if _, err := tool.Call(context.Background(), raw, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestReadFileOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	tool := &ReadFile{sandbox: newTestSandbox(t, root)}
	raw := json.RawMessage(`{"path":` + jsonString(filepath.Join(outside, "x")) + `}`)
	_, err := tool.Call(context.Background(), raw, nil)
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("err = %v, want ErrOutsideRoots", err)
	}
}

func TestRegistrySpecsSorted(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root)
	reg := DefaultRegistry(sb)
	var names []string
	for _, spec := range reg.Specs() {
		names = append(names, spec.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("specs not sorted: %v", names)
	}
	if len(names) != 3 {
		t.Fatalf("specs = %v", names)
	}
}

func TestRegistryUnknownTool(t *testing.T) {
	root := t.TempDir()
	reg := DefaultRegistry(newTestSandbox(t, root))
	_, err := reg.Call(context.Background(), "nope", nil, nil)
	var unknown *UnknownToolError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want UnknownToolError", err)
	}
}

func TestSystemInfo(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root)
	tool := &SystemInfo{sandbox: sb}
	res, err := tool.Call(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := res.StructuredContent.(systemInfoResult)
	if info.OS == "" || info.Arch == "" || info.TermioVersion == "" {
		t.Fatalf("result = %+v", info)
	}
	if info.Workspace != sb.Workspace() {
		t.Fatalf("workspace = %q, want %q", info.Workspace, sb.Workspace())
	}
	if len(info.AllowedRoots) != 1 || info.AllowedRoots[0] != sb.Workspace() {
		t.Fatalf("roots = %v, workspace = %s", info.AllowedRoots, sb.Workspace())
	}
}

func matchPaths(res protocol.CallToolResult) []string {
	sr := res.StructuredContent.(searchResult)
	paths := make([]string, len(sr.Matches))
	for i, m := range sr.Matches {
		paths[i] = m.Path
	}
	sort.Strings(paths)
	return paths
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
