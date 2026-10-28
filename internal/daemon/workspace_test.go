package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"termio/internal/config"
	"termio/internal/protocol"
	"termio/internal/tools"
)

func TestWorkspaceSetDefaultLookup(t *testing.T) {
	wsA := &Workspace{ID: "ws_a", Registry: tools.NewRegistry()}
	wsB := &Workspace{ID: "ws_b", Registry: tools.NewRegistry()}
	set, err := NewWorkspaceSet("ws_b", wsA, wsB)
	if err != nil {
		t.Fatal(err)
	}
	got, err := set.Get("")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "ws_b" {
		t.Fatalf("default = %q, want ws_b", got.ID)
	}
	got, err = set.Get("ws_a")
	if err != nil || got.ID != "ws_a" {
		t.Fatalf("Get(ws_a) = %+v, %v", got, err)
	}
	if _, err := set.Get("ws_missing"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("missing workspace error = %v", err)
	}
	list := set.List()
	if len(list) != 2 || list[0].ID != "ws_a" || list[1].ID != "ws_b" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Name != "" {
		t.Fatalf("unexpected name: %+v", list[0])
	}
}

func TestWorkspaceSetSpecsUnion(t *testing.T) {
	toolA := &namedTool{name: "alpha"}
	toolB := &namedTool{name: "beta"}
	shared := &namedTool{name: "shared"}
	set, err := NewWorkspaceSet("ws_a",
		&Workspace{ID: "ws_a", Registry: tools.NewRegistry(toolA, shared)},
		&Workspace{ID: "ws_b", Registry: tools.NewRegistry(toolB, shared)},
	)
	if err != nil {
		t.Fatal(err)
	}
	specs := set.Specs()
	if len(specs) != 3 {
		t.Fatalf("specs = %+v, want alpha, shared, beta", specs)
	}
	names := []string{specs[0].Name, specs[1].Name, specs[2].Name}
	if names[0] != "alpha" || names[1] != "shared" || names[2] != "beta" {
		t.Fatalf("spec order = %v", names)
	}
}

type namedTool struct{ name string }

func (n *namedTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        n.name,
		Description: n.name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func (n *namedTool) Call(context.Context, json.RawMessage, tools.ProgressFunc) (protocol.CallToolResult, error) {
	return protocol.CallToolResult{}, nil
}

func TestWorkspaceSetRejectsInvalid(t *testing.T) {
	if _, err := NewWorkspaceSet("", &Workspace{ID: "", Registry: tools.NewRegistry()}); err == nil {
		t.Fatal("empty id should fail")
	}
	if _, err := NewWorkspaceSet("ws_a", &Workspace{ID: "ws_a"}); err == nil {
		t.Fatal("nil registry should fail")
	}
	if _, err := NewWorkspaceSet("ws_missing", &Workspace{ID: "ws_a", Registry: tools.NewRegistry()}); err == nil {
		t.Fatal("missing default should fail")
	}
	dup := &Workspace{ID: "ws_a", Registry: tools.NewRegistry()}
	if _, err := NewWorkspaceSet("ws_a", dup, dup); err == nil {
		t.Fatal("duplicate id should fail")
	}
}

func TestLoadWorkspaces(t *testing.T) {
	t.Setenv("TERMIO_HOME", t.TempDir())
	rootA := t.TempDir()
	rootB := t.TempDir()
	extra := t.TempDir()
	cfg := &config.Config{
		Workspaces: []config.WorkspaceConfig{
			{ID: "ws_a", Name: "alpha", Path: rootA},
			{ID: "ws_b", Name: "beta", Path: rootB, ExtraRoots: []string{extra}},
		},
		DefaultWorkspaceID: "ws_a",
		MaxConcurrentCalls: 3,
		LogFile:            filepath.Join(t.TempDir(), "termio.log"),
	}
	set, err := LoadWorkspaces(cfg, filepath.Join(t.TempDir(), "config.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()

	if set.DefaultID() != "ws_a" || set.Default().ID != "ws_a" {
		t.Fatalf("default = %q", set.DefaultID())
	}
	wsB, err := set.Get("ws_b")
	if err != nil {
		t.Fatal(err)
	}
	wantB, err := filepath.EvalSymlinks(rootB)
	if err != nil {
		t.Fatal(err)
	}
	if wsB.Sandbox.Workspace() != wantB {
		t.Fatalf("workspace B path = %q, want %q", wsB.Sandbox.Workspace(), wantB)
	}
	if cap(wsB.Sem) != 3 {
		t.Fatalf("semaphore capacity = %d, want 3", cap(wsB.Sem))
	}
	roots := wsB.Sandbox.Roots()
	wantExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 || !strings.EqualFold(filepath.Clean(roots[1]), filepath.Clean(wantExtra)) {
		t.Fatalf("roots = %v, want workspace + %s", roots, wantExtra)
	}

	appData, err := config.AppDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appData, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(appData, "config.json")
	if err := os.WriteFile(secret, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := wsB.Sandbox.Resolve(secret); !errors.Is(err, tools.ErrDeniedPath) {
		t.Fatalf("app data file resolve error = %v, want ErrDeniedPath", err)
	}
}

func TestLoadWorkspacesClosesOnFailure(t *testing.T) {
	t.Setenv("TERMIO_HOME", t.TempDir())
	cfg := &config.Config{
		Workspaces: []config.WorkspaceConfig{
			{ID: "ws_a", Path: t.TempDir()},
			{ID: "ws_missing", Path: filepath.Join(t.TempDir(), "does-not-exist")},
		},
		DefaultWorkspaceID: "ws_a",
		MaxConcurrentCalls: 1,
	}
	if _, err := LoadWorkspaces(cfg, "", nil); err == nil {
		t.Fatal("expected missing-directory error")
	}
}
