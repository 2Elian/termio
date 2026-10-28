package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"termio/internal/config"
	"termio/internal/daemon"
	"termio/internal/gateway"
	"termio/internal/protocol"
	"termio/internal/tools"
)

const testToken = "test-token"

type e2eEnv struct {
	baseURL  string
	deviceID string
	root     string
	gw       *gateway.Server
	cancel   context.CancelFunc
}

type e2eWorkspace struct {
	ID       string
	Name     string
	Root     string
	Registry *tools.Registry
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func startE2E(t *testing.T, registry *tools.Registry, callTimeoutMS int, root string) *e2eEnv {
	t.Helper()
	return startE2EMulti(t, callTimeoutMS, config.DefaultWorkspaceID, []e2eWorkspace{{
		ID:       config.DefaultWorkspaceID,
		Name:     "default",
		Root:     root,
		Registry: registry,
	}})
}

func startE2EMulti(t *testing.T, callTimeoutMS int, defaultID string, workspaces []e2eWorkspace) *e2eEnv {
	t.Helper()
	gw, err := gateway.New(gateway.Options{
		Token:             testToken,
		Logger:            newLogger(),
		HeartbeatInterval: 5 * time.Second,
		CallTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())

	cfg := &config.Config{
		GatewayURL:         "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
		DeviceID:           "dev_test",
		DeviceName:         "test-device",
		Token:              testToken,
		DefaultWorkspaceID: defaultID,
		MaxConcurrentCalls: 4,
		CallTimeoutMS:      callTimeoutMS,
		LogLevel:           "info",
	}
	defs := make([]*daemon.Workspace, 0, len(workspaces))
	for _, w := range workspaces {
		cfg.Workspaces = append(cfg.Workspaces, config.WorkspaceConfig{ID: w.ID, Name: w.Name, Path: w.Root})
		defs = append(defs, &daemon.Workspace{
			ID:       w.ID,
			Name:     w.Name,
			Registry: w.Registry,
			Sem:      make(chan struct{}, 1),
		})
	}
	wsSet, err := daemon.NewWorkspaceSet(defaultID, defs...)
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(cfg, newLogger(), wsSet)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	root := workspaces[0].Root
	env := &e2eEnv{
		baseURL:  ts.URL,
		deviceID: cfg.DeviceID,
		root:     root,
		gw:       gw,
		cancel:   cancel,
	}
	t.Cleanup(func() {
		cancel()
		gw.Close()
		ts.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	waitForDevice(t, env.baseURL, env.deviceID, 5*time.Second)
	return env
}

func waitForDevice(t *testing.T, baseURL, deviceID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		devices, err := listDevices(baseURL)
		if err == nil {
			for _, d := range devices {
				if d == deviceID {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("device %s did not connect", deviceID)
}

func listDevices(baseURL string) ([]string, error) {
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var body struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, len(body.Devices))
	for i, d := range body.Devices {
		ids[i] = d.ID
	}
	return ids, nil
}

func callTool(t *testing.T, env *e2eEnv, name string, args any, opts map[string]any) (*http.Response, protocol.CallToolResult) {
	t.Helper()
	status, raw := callToolRaw(t, env, name, args, opts)
	resp := &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(raw))}
	var result protocol.CallToolResult
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode result: %v\n%s", err, raw)
		}
	}
	return resp, result
}

// callToolRaw sends one call and returns the status and raw body. It closes
// the response body before returning.
func callToolRaw(t *testing.T, env *e2eEnv, name string, args any, opts map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"name": name, "arguments": args}
	for k, v := range opts {
		body[k] = v
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, env.baseURL+"/v1/devices/"+env.deviceID+"/tools/call", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestE2ESearchReadAndSystemInfo(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"a.txt":     "alpha",
		"sub/b.txt": "beta",
		"c.log":     "gamma",
	})
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	env := startE2E(t, tools.DefaultRegistry(sb), 5000, root)

	resp, result := callTool(t, env, "search_files", map[string]any{"pattern": "*.txt"}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if result.IsError {
		t.Fatalf("tool error: %+v", result)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	var search struct {
		Matches []struct {
			Path string `json:"path"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(structured, &search); err != nil {
		t.Fatal(err)
	}
	if len(search.Matches) != 2 {
		t.Fatalf("matches = %+v", search.Matches)
	}
	if !strings.HasSuffix(search.Matches[0].Path, "a.txt") && !strings.HasSuffix(search.Matches[1].Path, "a.txt") {
		t.Fatalf("a.txt not found: %+v", search.Matches)
	}

	resp, result = callTool(t, env, "read_file", map[string]any{"path": filepath.Join(sb.Roots()[0], "a.txt")}, nil)
	if resp.StatusCode != http.StatusOK || result.IsError {
		t.Fatalf("read_file status=%d result=%+v", resp.StatusCode, result)
	}
	structured, _ = json.Marshal(result.StructuredContent)
	var read struct {
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(structured, &read); err != nil {
		t.Fatal(err)
	}
	if read.Encoding != "utf-8" {
		t.Fatalf("encoding = %q", read.Encoding)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "alpha" {
		t.Fatalf("content = %+v", result.Content)
	}

	resp, result = callTool(t, env, "system_info", map[string]any{}, nil)
	if resp.StatusCode != http.StatusOK || result.IsError {
		t.Fatalf("system_info status=%d result=%+v", resp.StatusCode, result)
	}
}

func TestE2EOutsideRootIsToolError(t *testing.T) {
	root := t.TempDir()
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	env := startE2E(t, tools.DefaultRegistry(sb), 5000, root)

	outside := t.TempDir()
	resp, result := callTool(t, env, "read_file", map[string]any{"path": filepath.Join(outside, "secret")}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !result.IsError {
		t.Fatalf("expected isError, got %+v", result)
	}
}

func TestE2EUnknownToolIsProtocolError(t *testing.T) {
	root := t.TempDir()
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	env := startE2E(t, tools.DefaultRegistry(sb), 5000, root)

	resp, _ := callTool(t, env, "does_not_exist", map[string]any{}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2EDuplicateCallIDReplaysCache(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "alpha"})
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	env := startE2E(t, tools.DefaultRegistry(sb), 5000, root)

	args := map[string]any{"pattern": "*.txt"}
	resp, result := callTool(t, env, "search_files", args, map[string]any{"call_id": "call-dup-1"})
	if resp.StatusCode != http.StatusOK || result.IsError {
		t.Fatalf("first call status=%d result=%+v", resp.StatusCode, result)
	}

	if err := os.Remove(filepath.Join(sb.Roots()[0], "a.txt")); err != nil {
		t.Fatal(err)
	}
	resp, result = callTool(t, env, "search_files", args, map[string]any{"call_id": "call-dup-1"})
	if resp.StatusCode != http.StatusOK || result.IsError {
		t.Fatalf("second call status=%d result=%+v", resp.StatusCode, result)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	if !strings.Contains(string(structured), "a.txt") {
		t.Fatalf("cached result not replayed: %s", structured)
	}
}

func newTestRegistry(t *testing.T, root string) *tools.Registry {
	t.Helper()
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return tools.DefaultRegistry(sb)
}

func callErrorCode(t *testing.T, raw []byte) int {
	t.Helper()
	var body struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, raw)
	}
	return body.Error.Code
}

func TestE2EWorkspaceRoutingAndIsolation(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFiles(t, rootA, map[string]string{"a.txt": "alpha"})
	writeFiles(t, rootB, map[string]string{"b.txt": "beta"})
	env := startE2EMulti(t, 5000, "ws_a", []e2eWorkspace{
		{ID: "ws_a", Name: "A", Root: rootA, Registry: newTestRegistry(t, rootA)},
		{ID: "ws_b", Name: "B", Root: rootB, Registry: newTestRegistry(t, rootB)},
	})

	// No workspace_id: the gateway fills in the device's default workspace.
	_, result := callTool(t, env, "system_info", map[string]any{}, nil)
	if result.IsError {
		t.Fatalf("system_info failed: %+v", result)
	}
	if result.Meta.WorkspaceID != "ws_a" {
		t.Fatalf("default workspace meta = %q, want ws_a", result.Meta.WorkspaceID)
	}
	structured, _ := json.Marshal(result.StructuredContent)
	var info struct {
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(structured, &info); err != nil {
		t.Fatal(err)
	}
	wantA, _ := filepath.EvalSymlinks(rootA)
	if !strings.EqualFold(filepath.Clean(info.Workspace), filepath.Clean(wantA)) {
		t.Fatalf("system_info workspace = %q, want %q", info.Workspace, wantA)
	}

	_, result = callTool(t, env, "search_files", map[string]any{"pattern": "*.txt"}, map[string]any{
		"workspace_id": "ws_b",
		"session_id":   "chat-b",
	})
	if result.IsError {
		t.Fatalf("search in B failed: %+v", result)
	}
	if result.Meta.WorkspaceID != "ws_b" || result.Meta.SessionID != "chat-b" {
		t.Fatalf("result meta = %+v", result.Meta)
	}
	structured, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(structured), "b.txt") || strings.Contains(string(structured), "a.txt") {
		t.Fatalf("search results crossed workspaces: %s", structured)
	}

	// Relative paths resolve inside the selected workspace.
	_, result = callTool(t, env, "read_file", map[string]any{"path": "b.txt"}, map[string]any{"workspace_id": "ws_b"})
	if result.IsError || len(result.Content) == 0 || result.Content[0].Text != "beta" {
		t.Fatalf("read_file in B = %+v", result)
	}
	_, result = callTool(t, env, "read_file", map[string]any{"path": "b.txt"}, map[string]any{"workspace_id": "ws_a"})
	if !result.IsError {
		t.Fatal("reading B's file from workspace A should fail")
	}

	status, raw := callToolRaw(t, env, "system_info", map[string]any{}, map[string]any{"workspace_id": "ws_missing"})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown workspace status = %d, body = %s", status, raw)
	}
	if code := callErrorCode(t, raw); code != protocol.CodeUnknownWorkspace {
		t.Fatalf("unknown workspace code = %d, want %d", code, protocol.CodeUnknownWorkspace)
	}
}

func TestE2ECallIDScopedByWorkspaceAndSession(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFiles(t, rootA, map[string]string{"a.txt": "alpha"})
	writeFiles(t, rootB, map[string]string{"b.txt": "beta"})
	env := startE2EMulti(t, 5000, "ws_a", []e2eWorkspace{
		{ID: "ws_a", Name: "A", Root: rootA, Registry: newTestRegistry(t, rootA)},
		{ID: "ws_b", Name: "B", Root: rootB, Registry: newTestRegistry(t, rootB)},
	})

	args := map[string]any{"pattern": "*.txt"}
	opts := map[string]any{"call_id": "same-call", "workspace_id": "ws_a", "session_id": "s1"}
	_, first := callTool(t, env, "search_files", args, opts)
	if first.IsError {
		t.Fatalf("first call: %+v", first)
	}

	// Same call id, different workspace: must execute against B, not replay A.
	optsB := map[string]any{"call_id": "same-call", "workspace_id": "ws_b", "session_id": "s1"}
	_, second := callTool(t, env, "search_files", args, optsB)
	if second.IsError {
		t.Fatalf("second call: %+v", second)
	}
	structured, _ := json.Marshal(second.StructuredContent)
	if !strings.Contains(string(structured), "b.txt") || strings.Contains(string(structured), "a.txt") {
		t.Fatalf("workspace B replayed workspace A's cache: %s", structured)
	}

	// Same call id and workspace, different session: independent execution.
	if err := os.Remove(filepath.Join(rootA, "a.txt")); err != nil {
		t.Fatal(err)
	}
	optsS2 := map[string]any{"call_id": "same-call", "workspace_id": "ws_a", "session_id": "s2"}
	_, third := callTool(t, env, "search_files", args, optsS2)
	if third.IsError {
		t.Fatalf("third call: %+v", third)
	}
	structured, _ = json.Marshal(third.StructuredContent)
	if strings.Contains(string(structured), "a.txt") {
		t.Fatalf("session s2 replayed session s1's cache: %s", structured)
	}

	// Retrying the original key still replays the original result.
	_, replay := callTool(t, env, "search_files", args, opts)
	structured, _ = json.Marshal(replay.StructuredContent)
	if !strings.Contains(string(structured), "a.txt") {
		t.Fatalf("original call id did not replay: %s", structured)
	}
}

type gateTool struct {
	name    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        g.name,
		Description: "blocks until released",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (g *gateTool) Call(ctx context.Context, _ json.RawMessage, _ tools.ProgressFunc) (protocol.CallToolResult, error) {
	g.once.Do(func() { close(g.started) })
	select {
	case <-g.release:
		return protocol.CallToolResult{
			Content: []protocol.ContentBlock{{Type: "text", Text: g.name + " done"}},
		}, nil
	case <-ctx.Done():
		return protocol.CallToolResult{}, ctx.Err()
	}
}

func TestE2EWorkspacesExecuteConcurrently(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	toolA := &gateTool{name: "gate_a", started: make(chan struct{}), release: make(chan struct{})}
	toolB := &gateTool{name: "gate_b", started: make(chan struct{}), release: make(chan struct{})}
	env := startE2EMulti(t, 5000, "ws_a", []e2eWorkspace{
		{ID: "ws_a", Name: "A", Root: rootA, Registry: tools.NewRegistry(toolA)},
		{ID: "ws_b", Name: "B", Root: rootB, Registry: tools.NewRegistry(toolB)},
	})

	type outcome struct {
		status int
		result protocol.CallToolResult
		err    error
	}
	results := make(chan outcome, 2)
	start := func(workspaceID, tool string) {
		go func() {
			body, _ := json.Marshal(map[string]any{
				"name":         tool,
				"arguments":    map[string]any{},
				"workspace_id": workspaceID,
				"timeout_ms":   5000,
			})
			req, err := http.NewRequest(http.MethodPost, env.baseURL+"/v1/devices/"+env.deviceID+"/tools/call", bytes.NewReader(body))
			if err != nil {
				results <- outcome{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- outcome{err: err}
				return
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			var result protocol.CallToolResult
			if err == nil && resp.StatusCode == http.StatusOK {
				err = json.Unmarshal(raw, &result)
			}
			results <- outcome{status: resp.StatusCode, result: result, err: err}
		}()
	}
	start("ws_a", "gate_a")
	start("ws_b", "gate_b")

	// Each workspace allows one in-flight call in this test. Both starting at
	// the same time proves the semaphores are per workspace, not global.
	for name, ch := range map[string]<-chan struct{}{"A": toolA.started, "B": toolB.started} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("workspace %s call did not start while the other was running", name)
		}
	}
	close(toolA.release)
	close(toolB.release)

	for i := 0; i < 2; i++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.status != http.StatusOK || got.result.IsError {
				t.Fatalf("call failed: status=%d result=%+v", got.status, got.result)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent calls did not complete")
		}
	}
}

func TestE2EToolsRefreshUsesDaemonToolsList(t *testing.T) {
	root := t.TempDir()
	env := startE2E(t, newTestRegistry(t, root), 5000, root)

	req, _ := http.NewRequest(http.MethodGet, env.baseURL+"/v1/devices/"+env.deviceID+"/tools?refresh=1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var body struct {
		Tools       []protocol.ToolSpec `json:"tools"`
		MaxInflight int                 `json:"max_inflight"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 3 {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if body.MaxInflight != 4 {
		t.Fatalf("max_inflight = %d, want 4", body.MaxInflight)
	}
}

func TestE2EWorkspacesEndpoint(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	env := startE2EMulti(t, 5000, "ws_b", []e2eWorkspace{
		{ID: "ws_a", Name: "A", Root: rootA, Registry: newTestRegistry(t, rootA)},
		{ID: "ws_b", Name: "B", Root: rootB, Registry: newTestRegistry(t, rootB)},
	})

	req, _ := http.NewRequest(http.MethodGet, env.baseURL+"/v1/devices/"+env.deviceID+"/workspaces", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		DeviceID           string                   `json:"device_id"`
		Workspaces         []protocol.WorkspaceInfo `json:"workspaces"`
		DefaultWorkspaceID string                   `json:"default_workspace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.DeviceID != env.deviceID || body.DefaultWorkspaceID != "ws_b" {
		t.Fatalf("body = %+v", body)
	}
	if len(body.Workspaces) != 2 || body.Workspaces[0].ID != "ws_a" || body.Workspaces[1].ID != "ws_b" {
		t.Fatalf("workspaces = %+v", body.Workspaces)
	}
	if strings.Contains(fmt.Sprint(body), rootA) || strings.Contains(fmt.Sprint(body), rootB) {
		t.Fatalf("workspace paths must stay local: %+v", body)
	}
}

type slowTool struct {
	cancelled chan struct{}
	once      sync.Once
}

func (s *slowTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        "slow_tool",
		Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}
}

func (s *slowTool) Call(ctx context.Context, _ json.RawMessage, _ tools.ProgressFunc) (protocol.CallToolResult, error) {
	select {
	case <-time.After(5 * time.Second):
		return protocol.CallToolResult{Content: []protocol.ContentBlock{{Type: "text", Text: "done"}}}, nil
	case <-ctx.Done():
		s.once.Do(func() { close(s.cancelled) })
		return protocol.CallToolResult{}, ctx.Err()
	}
}

func TestE2EToolTimeoutIsResult(t *testing.T) {
	root := t.TempDir()
	slow := &slowTool{cancelled: make(chan struct{})}
	env := startE2E(t, tools.NewRegistry(slow), 1000, root)

	resp, result := callTool(t, env, "slow_tool", map[string]any{}, map[string]any{"timeout_ms": 1000})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !result.IsError {
		t.Fatalf("expected isError result, got %+v", result)
	}
	select {
	case <-slow.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool context was not canceled")
	}
}

func TestE2EClientDisconnectCancelsTool(t *testing.T) {
	root := t.TempDir()
	slow := &slowTool{cancelled: make(chan struct{})}
	env := startE2E(t, tools.NewRegistry(slow), 10000, root)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	body := []byte(`{"name":"slow_tool","arguments":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.baseURL+"/v1/devices/"+env.deviceID+"/tools/call", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the client request to be canceled")
	}
	select {
	case <-slow.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("tool was not canceled after the client disconnected")
	}
}

func TestE2EReconnectAfterSessionClosed(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "alpha"})
	sb, err := tools.NewSandbox(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	env := startE2E(t, tools.DefaultRegistry(sb), 5000, root)

	// Drop the session while leaving the HTTP server running. The daemon must
	// redial and register again.
	env.gw.Close()

	deadline := time.Now().Add(6 * time.Second)
	for {
		resp, result := callTool(t, env, "system_info", map[string]any{}, nil)
		if resp.StatusCode == http.StatusOK && !result.IsError {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not reconnect; last status=%d result=%+v", resp.StatusCode, result)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestE2EFakeDeviceManualProtocol mirrors the Postman WebSocket walkthrough:
// a hand-written client registers as a device, receives the pushed tools/call,
// and replies with a result. It proves the documented manual frames work.
func TestE2EFakeDeviceManualProtocol(t *testing.T) {
	gw, err := gateway.New(gateway.Options{
		Token:             testToken,
		Logger:            newLogger(),
		HeartbeatInterval: 5 * time.Second,
		CallTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	defer ts.Close()
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+testToken)
	header.Set("X-Termio-Device", "dev_postman")
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	reg, err := protocol.NewRequest("reg-1", protocol.MethodRegister, protocol.RegisterParams{
		ProtocolVersion: protocol.ProtocolVersion,
		MCPVersion:      protocol.MCPVersion,
		Device: protocol.DeviceInfo{
			ID: "dev_postman", Name: "postman-fake", OS: "test", Arch: "test", Version: "0",
		},
		Tools: []protocol.ToolSpec{{
			Name:        "echo",
			Description: "Echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		Workspaces: []protocol.WorkspaceInfo{
			{ID: "ws_docs", Name: "docs"},
			{ID: "ws_code", Name: "code"},
		},
		DefaultWorkspaceID: "ws_docs",
		MaxInflight:        4,
	})
	if err != nil {
		t.Fatal(err)
	}
	regData, _ := json.Marshal(reg)
	if err := conn.Write(ctx, websocket.MessageText, regData); err != nil {
		t.Fatal(err)
	}
	_, regData, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var regResp protocol.Message
	if err := json.Unmarshal(regData, &regResp); err != nil {
		t.Fatal(err)
	}
	if regResp.Error != nil {
		t.Fatalf("register failed: %v", regResp.Error)
	}

	waitForDevice(t, ts.URL, "dev_postman", 2*time.Second)

	type httpOutcome struct {
		status int
		body   []byte
		err    error
	}
	outcome := make(chan httpOutcome, 1)
	go func() {
		body := []byte(`{"name":"echo","arguments":{"message":"hi"},"timeout_ms":5000,"call_id":"fake-call-1"}`)
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/devices/dev_postman/tools/call", bytes.NewReader(body))
		if err != nil {
			outcome <- httpOutcome{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			outcome <- httpOutcome{err: err}
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		outcome <- httpOutcome{status: resp.StatusCode, body: raw, err: err}
	}()

	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	_, pushData, err := conn.Read(readCtx)
	readCancel()
	if err != nil {
		t.Fatal(err)
	}
	var push protocol.Message
	if err := json.Unmarshal(pushData, &push); err != nil {
		t.Fatal(err)
	}
	if push.Method != protocol.MethodToolsCall || push.ID == "" {
		t.Fatalf("unexpected push: %s", pushData)
	}
	var params protocol.ToolCallParams
	if err := protocol.DecodeParams(&push, &params); err != nil {
		t.Fatal(err)
	}
	if params.Name != "echo" || params.Meta.CallID != "fake-call-1" {
		t.Fatalf("unexpected params: %+v", params)
	}
	if params.Meta.WorkspaceID != "ws_docs" {
		t.Fatalf("gateway did not inject the default workspace: %+v", params.Meta)
	}

	progress, err := protocol.NewNotification(protocol.MethodProgress, protocol.ProgressParams{
		Progress: 1,
		Total:    2,
		Message:  "working",
		Meta:     protocol.CallMeta{CallID: params.Meta.CallID},
	})
	if err != nil {
		t.Fatal(err)
	}
	progressData, _ := json.Marshal(progress)
	if err := conn.Write(ctx, websocket.MessageText, progressData); err != nil {
		t.Fatal(err)
	}

	result, err := protocol.NewResult(push.ID, protocol.CallToolResult{
		Content: []protocol.ContentBlock{{Type: "text", Text: "hi from fake device"}},
		Meta:    protocol.CallMeta{CallID: params.Meta.CallID},
	})
	if err != nil {
		t.Fatal(err)
	}
	resultData, _ := json.Marshal(result)
	if err := conn.Write(ctx, websocket.MessageText, resultData); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-outcome:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, body = %s", got.status, got.body)
		}
		var callResult protocol.CallToolResult
		if err := json.Unmarshal(got.body, &callResult); err != nil {
			t.Fatal(err)
		}
		if len(callResult.Content) != 1 || callResult.Content[0].Text != "hi from fake device" {
			t.Fatalf("unexpected result: %+v", callResult)
		}
	case <-ctx.Done():
		t.Fatal("HTTP call did not complete")
	}
}

func TestGatewayRejectsWorkspaceForLegacyDevice(t *testing.T) {
	gw, err := gateway.New(gateway.Options{
		Token:             testToken,
		Logger:            newLogger(),
		HeartbeatInterval: 5 * time.Second,
		CallTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	defer ts.Close()
	defer gw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+testToken)
	header.Set("X-Termio-Device", "dev_legacy")
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	reg, err := protocol.NewRequest("reg-legacy", protocol.MethodRegister, protocol.RegisterParams{
		ProtocolVersion: protocol.ProtocolVersion,
		Device:          protocol.DeviceInfo{ID: "dev_legacy", OS: "test", Arch: "test", Version: "0"},
		Tools: []protocol.ToolSpec{{
			Name:        "echo",
			Description: "Echo",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	regData, _ := json.Marshal(reg)
	if err := conn.Write(ctx, websocket.MessageText, regData); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	waitForDevice(t, ts.URL, "dev_legacy", 2*time.Second)

	// A legacy device registers no workspaces. A workspace-scoped call must be
	// rejected instead of silently running in its sole workspace.
	body := []byte(`{"name":"echo","arguments":{},"workspace_id":"ws_docs"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/devices/dev_legacy/tools/call", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if code := callErrorCode(t, raw); code != protocol.CodeUnknownWorkspace {
		t.Fatalf("code = %d, want %d", code, protocol.CodeUnknownWorkspace)
	}
}

func TestGatewayAuth(t *testing.T) {
	gw, err := gateway.New(gateway.Options{Token: testToken, Logger: newLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	defer ts.Close()
	defer gw.Close()

	resp, err := http.Get(ts.URL + "/v1/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}

	if _, err := gateway.New(gateway.Options{}); err == nil {
		t.Fatal("gateway without auth should fail to construct")
	}
}
