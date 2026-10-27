package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestSandbox(t *testing.T, workspace string, extras ...string) *Sandbox {
	t.Helper()
	sb, err := NewSandbox(workspace, extras)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func TestResolveInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "sub", "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sb := newTestSandbox(t, root)
	resolved, err := sb.Resolve(file)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := "sub/file.txt"
	if resolved.RelSlash != wantRel {
		t.Fatalf("RelSlash = %q, want %q", resolved.RelSlash, wantRel)
	}
	if filepath.FromSlash(resolved.RelSlash) != resolved.RelOS {
		t.Fatalf("RelOS = %q", resolved.RelOS)
	}
	if want := filepath.Join(sb.Roots()[0], "sub", "file.txt"); resolved.Abs != want {
		t.Fatalf("Abs = %q, want %q", resolved.Abs, want)
	}
}

func TestResolveOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sb := newTestSandbox(t, root)
	_, err := sb.Resolve(filepath.Join(outside, "secret.txt"))
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("err = %v, want ErrOutsideRoots", err)
	}
}

func TestResolveTraversal(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root)
	_, err := sb.Resolve(filepath.Join(root, "..", "escape"))
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("err = %v, want ErrOutsideRoots", err)
	}
}

func TestResolveRelativeUnderWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "sub", "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb := newTestSandbox(t, root)
	resolved, err := sb.Resolve("sub/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(sb.Roots()[0], "sub", "file.txt"); resolved.Abs != want {
		t.Fatalf("Abs = %q, want %q", resolved.Abs, want)
	}
}

func TestResolveEmptyUsesWorkspace(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root)
	resolved, err := sb.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RelSlash != "." || resolved.Abs != sb.Workspace() {
		t.Fatalf("resolved = %+v, workspace = %s", resolved, sb.Workspace())
	}
}

func TestResolveRelativeEscapeRejected(t *testing.T) {
	sb := newTestSandbox(t, t.TempDir())
	_, err := sb.Resolve("../outside")
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestWorkspaceAndExtraRoots(t *testing.T) {
	workspace := t.TempDir()
	extra := t.TempDir()
	file := filepath.Join(extra, "shared.txt")
	if err := os.WriteFile(file, []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb := newTestSandbox(t, workspace, extra)
	roots := sb.Roots()
	if len(roots) != 2 || roots[0] != sb.Workspace() {
		t.Fatalf("roots = %v, want workspace first + extra", roots)
	}
	if _, err := sb.Resolve(file); err != nil {
		t.Fatalf("absolute extra root should resolve: %v", err)
	}
	if _, err := sb.Resolve(filepath.Join("..", filepath.Base(extra), "shared.txt")); err == nil {
		t.Fatal("relative path should not escape the workspace")
	}
}

func TestResolvePathTooLong(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root)
	_, err := sb.Resolve(filepath.Join(root, strings.Repeat("a", 4096)))
	var argErr *ArgError
	if !errors.As(err, &argErr) {
		t.Fatalf("err = %v, want ArgError", err)
	}
}

func TestSymlinkEscapeBlockedAtOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	sb := newTestSandbox(t, root)
	resolved, err := sb.Resolve(link)
	if err == nil {
		if _, openErr := resolved.Entry.root.Open(resolved.RelOS); openErr == nil {
			t.Fatal("os.Root allowed a symlink escape")
		}
	} else if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("unexpected resolve error: %v", err)
	}
}

func TestSymlinkInsideRootAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privileges on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	sb := newTestSandbox(t, root)
	resolved, err := sb.Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	f, err := resolved.Entry.root.Open(resolved.RelOS)
	if err != nil {
		t.Fatalf("symlink inside root should open: %v", err)
	}
	f.Close()
}

func TestDuplicateRootsDeduped(t *testing.T) {
	root := t.TempDir()
	sb := newTestSandbox(t, root, root)
	if got := len(sb.Roots()); got != 1 {
		t.Fatalf("roots = %d, want 1", got)
	}
}

func TestWindowsCaseInsensitiveResolve(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific")
	}
	root := t.TempDir()
	file := filepath.Join(root, "CaseFile.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb := newTestSandbox(t, root)
	if _, err := sb.Resolve(strings.ToUpper(file)); err != nil {
		t.Fatalf("case-insensitive resolve failed: %v", err)
	}
}
