package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	dir := t.TempDir()
	return &Config{
		GatewayURL:         "ws://127.0.0.1:8787/ws",
		DeviceID:           "dev_0123456789abcdef0123456789abcdef",
		DeviceName:         "test-device",
		Token:              "test-token",
		Workspace:          dir,
		MaxConcurrentCalls: 4,
		CallTimeoutMS:      30000,
		LogLevel:           "info",
		LogFile:            filepath.Join(dir, "termio.log"),
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	cfg := validConfig(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Token != cfg.Token || loaded.DeviceID != cfg.DeviceID {
		t.Fatalf("credentials did not round-trip: %+v", loaded)
	}
	if loaded.MaxConcurrentCalls != 4 || loaded.CallTimeoutMS != 30000 {
		t.Fatalf("defaults did not round-trip: %+v", loaded)
	}
	if loaded.Workspace != cfg.Workspace || !filepath.IsAbs(loaded.Workspace) {
		t.Fatalf("workspace = %q, want %q", loaded.Workspace, cfg.Workspace)
	}
	if len(loaded.AllowedRoots) != 0 {
		t.Fatalf("extra roots = %v, want none", loaded.AllowedRoots)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("config mode = %o, want 600", perm)
		}
	}
}

func TestSaveDedupesWorkspaceFromExtras(t *testing.T) {
	cfg := validConfig(t)
	extra := t.TempDir()
	cfg.AllowedRoots = []string{cfg.Workspace, extra, extra}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.AllowedRoots) != 1 {
		t.Fatalf("extra roots = %v, want one", loaded.AllowedRoots)
	}
	wantExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AllowedRoots[0] != wantExtra {
		t.Fatalf("extra root = %q, want %q", loaded.AllowedRoots[0], wantExtra)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"gateway_url":"ws://127.0.0.1:8787/ws","device_id":"dev_x","token":"t","typo_field":true}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadAppliesDefaultsAndMigratesLegacyRoots(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{
		"gateway_url":   "ws://127.0.0.1:8787/ws",
		"device_id":     "dev_x",
		"token":         "t",
		"allowed_roots": []string{dir1, dir2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentCalls != DefaultMaxConcurrent {
		t.Fatalf("max_concurrent_calls = %d", cfg.MaxConcurrentCalls)
	}
	if cfg.CallTimeoutMS != DefaultCallTimeoutMS {
		t.Fatalf("call_timeout_ms = %d", cfg.CallTimeoutMS)
	}
	if cfg.LogLevel != "info" {
		t.Fatalf("log_level = %q", cfg.LogLevel)
	}
	if cfg.DeviceName == "" {
		t.Fatal("device_name should default to hostname")
	}
	wantWorkspace, err := filepath.EvalSymlinks(dir1)
	if err != nil {
		t.Fatal(err)
	}
	wantExtra, err := filepath.EvalSymlinks(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != wantWorkspace {
		t.Fatalf("workspace = %q, want %q", cfg.Workspace, wantWorkspace)
	}
	if len(cfg.AllowedRoots) != 1 || cfg.AllowedRoots[0] != wantExtra {
		t.Fatalf("extras = %v, want [%s]", cfg.AllowedRoots, wantExtra)
	}
}

func TestValidate(t *testing.T) {
	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(c *Config) {}, false},
		{"wss remote", func(c *Config) { c.GatewayURL = "wss://gateway.example.com/ws" }, false},
		{"no extra roots", func(c *Config) { c.AllowedRoots = nil }, false},
		{"missing device id", func(c *Config) { c.DeviceID = "" }, true},
		{"bad device id", func(c *Config) { c.DeviceID = "dev/bad" }, true},
		{"device id with space", func(c *Config) { c.DeviceID = "dev bad" }, true},
		{"missing token", func(c *Config) { c.Token = "  " }, true},
		{"http scheme", func(c *Config) { c.GatewayURL = "http://example.com" }, true},
		{"plain ws remote", func(c *Config) { c.GatewayURL = "ws://gateway.example.com/ws" }, true},
		{"missing workspace", func(c *Config) { c.Workspace = "" }, true},
		{"relative workspace", func(c *Config) { c.Workspace = "relative" }, true},
		{"file workspace", func(c *Config) { c.Workspace = fileRoot }, true},
		{"relative extra root", func(c *Config) { c.AllowedRoots = []string{"relative"} }, true},
		{"file extra root", func(c *Config) { c.AllowedRoots = []string{fileRoot} }, true},
		{"too much concurrency", func(c *Config) { c.MaxConcurrentCalls = MaxConcurrentCallsLimit + 1 }, true},
		{"timeout too small", func(c *Config) { c.CallTimeoutMS = MinCallTimeoutMS - 1 }, true},
		{"timeout too large", func(c *Config) { c.CallTimeoutMS = MaxCallTimeoutMS + 1 }, true},
		{"bad log level", func(c *Config) { c.LogLevel = "verbose" }, true},
		{"relative log file", func(c *Config) { c.LogFile = "termio.log" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig(t)
			tt.mutate(cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEffectiveRootsDedupes(t *testing.T) {
	cfg := validConfig(t)
	extra := t.TempDir()
	cfg.AllowedRoots = []string{cfg.Workspace, extra, extra}
	roots := cfg.EffectiveRoots()
	if len(roots) != 2 {
		t.Fatalf("roots = %v, want workspace + one extra", roots)
	}
	if roots[0] != cfg.Workspace || roots[1] != filepath.Clean(extra) {
		t.Fatalf("roots = %v", roots)
	}
}

func TestLoadDefaultMigratesLegacyConfig(t *testing.T) {
	base := t.TempDir()
	newHome := filepath.Join(base, "apphome")
	t.Setenv("TERMIO_HOME", newHome)
	t.Setenv("LARES_HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", filepath.Join(base, "roaming"))
		t.Setenv("LOCALAPPDATA", filepath.Join(base, "local"))
	} else {
		t.Setenv("HOME", base)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg-config"))
		t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "xdg-cache"))
	}

	legacyPath, err := LegacyDefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	legacyLog := LegacyDefaultLogPath()
	cfg := &Config{
		GatewayURL:         "ws://127.0.0.1:8787/ws",
		DeviceID:           "dev_0123456789abcdef0123456789abcdef",
		DeviceName:         "legacy",
		Token:              "legacy-token",
		Workspace:          workspace,
		MaxConcurrentCalls: 4,
		CallTimeoutMS:      30000,
		LogLevel:           "info",
		LogFile:            legacyLog,
	}
	if err := cfg.Save(legacyPath); err != nil {
		t.Fatal(err)
	}

	got, newPath, migrated, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if !migrated {
		t.Fatal("expected migration")
	}
	wantPath, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if newPath != wantPath {
		t.Fatalf("new path = %q, want %q", newPath, wantPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("migrated config not written: %v", err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace != wantWorkspace {
		t.Fatalf("workspace = %q, want %q", got.Workspace, wantWorkspace)
	}
	if got.LogFile != DefaultLogPath() {
		t.Fatalf("log file = %q, want %q", got.LogFile, DefaultLogPath())
	}
}

func TestIsBroadWorkspace(t *testing.T) {
	if home, err := os.UserHomeDir(); err == nil && !IsBroadWorkspace(home) {
		t.Fatalf("home %q should be broad", home)
	}
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = filepath.VolumeName(t.TempDir()) + `\`
	}
	if !IsBroadWorkspace(root) {
		t.Fatalf("filesystem root %q should be broad", root)
	}
	if IsBroadWorkspace(t.TempDir()) {
		t.Fatal("a temp directory should not be broad")
	}
}

func TestDefaultWorkspacePath(t *testing.T) {
	t.Setenv("TERMIO_HOME", "")
	t.Setenv("LARES_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := DefaultWorkspacePath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, DefaultHomeDirName); got != want {
		t.Fatalf("workspace = %q, want %q", got, want)
	}
}

func TestDefaultLocationsUseAppHome(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-lares")
	t.Setenv("TERMIO_HOME", custom)
	t.Setenv("LARES_HOME", "")

	home, err := AppHome()
	if err != nil {
		t.Fatal(err)
	}
	if home != filepath.Clean(custom) {
		t.Fatalf("AppHome = %q, want %q", home, filepath.Clean(custom))
	}
	data, err := AppDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, AppDataDirName); data != want {
		t.Fatalf("AppDataDir = %q, want %q", data, want)
	}
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(data, "config.json"); path != want {
		t.Fatalf("DefaultPath = %q, want %q", path, want)
	}
	if got, want := DefaultLogPath(), filepath.Join(data, "termio.log"); got != want {
		t.Fatalf("DefaultLogPath = %q, want %q", got, want)
	}
}

func TestNewDefaultUsesAppHome(t *testing.T) {
	t.Setenv("TERMIO_HOME", "")
	t.Setenv("LARES_HOME", "")
	cfg, err := NewDefault()
	if err != nil {
		t.Skipf("cannot build default config: %v", err)
	}
	if filepath.Base(cfg.Workspace) != DefaultHomeDirName {
		t.Fatalf("workspace = %q", cfg.Workspace)
	}
	if len(cfg.AllowedRoots) != 0 {
		t.Fatalf("extra roots = %v", cfg.AllowedRoots)
	}
}

func TestGenerateDeviceID(t *testing.T) {
	id1, err := GenerateDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := GenerateDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id1, "dev_") || len(id1) != len("dev_")+32 {
		t.Fatalf("id = %q", id1)
	}
	if id1 == id2 {
		t.Fatal("ids should be unique")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := ExpandHome("~/sub")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "sub"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, err := ExpandHome("relative"); err != nil || got != "relative" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRedactedToken(t *testing.T) {
	if got := RedactedToken(""); got != "" {
		t.Fatalf("empty token = %q", got)
	}
	if got := RedactedToken("short"); got != "***" {
		t.Fatalf("short token = %q", got)
	}
	got := RedactedToken("abcdefghijklmnop")
	if strings.Contains(got, "defghijk") {
		t.Fatalf("token not redacted: %q", got)
	}
}
