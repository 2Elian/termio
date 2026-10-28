// Package config loads and validates the local daemon configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	DefaultGatewayURL       = "ws://127.0.0.1:8787/ws"
	DefaultMaxConcurrent    = 4
	DefaultCallTimeoutMS    = 30000
	MinCallTimeoutMS        = 1000
	MaxCallTimeoutMS        = 300000
	MaxConcurrentCallsLimit = 32
	DefaultHomeDirName      = ".lares"
	AppDataDirName          = ".termio"
	DefaultWorkspaceID      = "ws_default"
	MaxWorkspaces           = 64
)

// WorkspaceConfig is one locally authorized workspace. ID is opaque to the
// cloud; Path is never sent in tool calls.
type WorkspaceConfig struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Path       string   `json:"path"`
	ExtraRoots []string `json:"extra_roots,omitempty"`
}

// Config is the on-disk daemon configuration.
type Config struct {
	GatewayURL         string            `json:"gateway_url"`
	DeviceID           string            `json:"device_id"`
	DeviceName         string            `json:"device_name"`
	Token              string            `json:"token"`
	Workspaces         []WorkspaceConfig `json:"workspaces,omitempty"`
	DefaultWorkspaceID string            `json:"default_workspace_id,omitempty"`
	MaxConcurrentCalls int               `json:"max_concurrent_calls"`
	CallTimeoutMS      int               `json:"call_timeout_ms"`
	LogLevel           string            `json:"log_level"`
	LogFile            string            `json:"log_file"`

	// Legacy single-workspace fields, read for migration and omitted on save.
	Workspace    string   `json:"workspace,omitempty"`
	AllowedRoots []string `json:"allowed_roots,omitempty"`
}

// AppHome returns the per-user Termio home. It defaults to ~/.lares and can
// be overridden with TERMIO_HOME or LARES_HOME.
func AppHome() (string, error) {
	for _, key := range []string{"TERMIO_HOME", "LARES_HOME"} {
		if raw := strings.TrimSpace(os.Getenv(key)); raw != "" {
			expanded, err := ExpandHome(raw)
			if err != nil {
				return "", err
			}
			abs, err := filepath.Abs(expanded)
			if err != nil {
				return "", fmt.Errorf("%s: %w", key, err)
			}
			return filepath.Clean(abs), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, DefaultHomeDirName), nil
}

// AppDataDir returns the directory for terminal metadata (config, logs).
// It is always inside the app home, even when the workspace is elsewhere.
func AppDataDir() (string, error) {
	home, err := AppHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, AppDataDirName), nil
}

// DefaultWorkspacePath returns the default workspace: the app home.
func DefaultWorkspacePath() (string, error) {
	return AppHome()
}

// DefaultPath returns the config path inside the app data directory.
func DefaultPath() (string, error) {
	dir, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// DefaultLogPath returns the log path inside the app data directory.
func DefaultLogPath() string {
	dir, err := AppDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "termio.log")
}

// LegacyDefaultPath is the pre-workspace config location.
func LegacyDefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "termio", "config.json"), nil
}

// LegacyDefaultLogPath is the pre-workspace log location.
func LegacyDefaultLogPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "termio", "termio.log")
}

// NewDefault builds a config with a fresh device ID, the default app home
// workspace, no extra roots, and the loopback development gateway URL.
// It does not create the workspace directory; callers that initialize a new
// install do that.
func NewDefault() (*Config, error) {
	id, err := GenerateDeviceID()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "termio-device"
	}
	workspace, err := DefaultWorkspacePath()
	if err != nil {
		return nil, err
	}
	return &Config{
		GatewayURL: DefaultGatewayURL,
		DeviceID:   id,
		DeviceName: host,
		Workspaces: []WorkspaceConfig{{
			ID:   DefaultWorkspaceID,
			Name: "default",
			Path: workspace,
		}},
		DefaultWorkspaceID: DefaultWorkspaceID,
		MaxConcurrentCalls: DefaultMaxConcurrent,
		CallTimeoutMS:      DefaultCallTimeoutMS,
		LogLevel:           "info",
		LogFile:            DefaultLogPath(),
	}, nil
}

// GenerateDeviceID returns dev_ followed by 32 random hex characters.
func GenerateDeviceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	return "dev_" + hex.EncodeToString(b[:]), nil
}

// Load reads, defaults, normalizes, and validates a config file. Unknown
// fields are rejected so typos do not silently weaken security settings.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse %s: trailing data", path)
	}

	c.applyDefaults()
	if err := c.normalizeWorkspaces(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// normalizeWorkspaces migrates the legacy single workspace and normalizes all
// workspace paths, IDs, and extra roots. It clears legacy fields so Save writes
// only the new schema.
func (c *Config) normalizeWorkspaces() error {
	if len(c.Workspaces) > MaxWorkspaces {
		return fmt.Errorf("too many workspaces (maximum %d)", MaxWorkspaces)
	}
	if len(c.Workspaces) == 0 {
		path := c.Workspace
		extras := append([]string(nil), c.AllowedRoots...)
		if path == "" && len(extras) > 0 {
			path = extras[0]
			extras = extras[1:]
		}
		if path == "" {
			defaultPath, err := DefaultWorkspacePath()
			if err != nil {
				return err
			}
			path = defaultPath
		}
		c.Workspaces = []WorkspaceConfig{{
			ID:         DefaultWorkspaceID,
			Name:       "default",
			Path:       path,
			ExtraRoots: extras,
		}}
		c.DefaultWorkspaceID = DefaultWorkspaceID
	}

	seen := make(map[string]bool, len(c.Workspaces))
	seenPaths := make(map[string]string, len(c.Workspaces))
	for i := range c.Workspaces {
		w := &c.Workspaces[i]
		if strings.TrimSpace(w.ID) == "" {
			return fmt.Errorf("workspaces[%d].id is required", i)
		}
		if !validWorkspaceID(w.ID) {
			return fmt.Errorf("workspace id %q must be 1-128 characters of letters, digits, dot, dash, or underscore", w.ID)
		}
		if seen[w.ID] {
			return fmt.Errorf("duplicate workspace id %q", w.ID)
		}
		seen[w.ID] = true
		if len(w.Name) > 128 {
			return fmt.Errorf("workspace %q name is too long", w.ID)
		}
		normalized, err := NormalizeWorkspace(w.Path)
		if err != nil {
			return fmt.Errorf("workspace %q: %w (run termio workspace add)", w.ID, err)
		}
		w.Path = normalized
		key := pathKey(normalized)
		if other, exists := seenPaths[key]; exists {
			return fmt.Errorf("workspaces %q and %q use the same path %q", other, w.ID, normalized)
		}
		seenPaths[key] = w.ID
		extras, err := NormalizeRoots(w.ExtraRoots)
		if err != nil {
			return fmt.Errorf("workspace %q extra_roots: %w", w.ID, err)
		}
		w.ExtraRoots = DedupeRoots(w.Path, extras)
	}

	if c.DefaultWorkspaceID == "" {
		c.DefaultWorkspaceID = c.Workspaces[0].ID
	}
	if !seen[c.DefaultWorkspaceID] {
		return fmt.Errorf("default_workspace_id %q is not in workspaces", c.DefaultWorkspaceID)
	}
	c.Workspace = ""
	c.AllowedRoots = nil
	return nil
}

// WorkspaceByID returns a workspace definition by ID.
func (c *Config) WorkspaceByID(id string) (WorkspaceConfig, bool) {
	for _, w := range c.Workspaces {
		if w.ID == id {
			return w, true
		}
	}
	return WorkspaceConfig{}, false
}

// DefaultWorkspace returns the configured default workspace.
func (c *Config) DefaultWorkspace() (WorkspaceConfig, bool) {
	return c.WorkspaceByID(c.DefaultWorkspaceID)
}

// FindWorkspaceByPath returns the workspace whose path resolves to path.
func (c *Config) FindWorkspaceByPath(path string) (WorkspaceConfig, bool) {
	normalized, err := NormalizeWorkspace(path)
	if err != nil {
		return WorkspaceConfig{}, false
	}
	for _, w := range c.Workspaces {
		if samePath(w.Path, normalized) {
			return w, true
		}
	}
	return WorkspaceConfig{}, false
}

// AddWorkspace appends a locally authorized workspace and returns it.
func (c *Config) AddWorkspace(path, name, id string, extraRoots []string) (WorkspaceConfig, error) {
	normalized, err := NormalizeWorkspace(path)
	if err != nil {
		return WorkspaceConfig{}, err
	}
	for _, w := range c.Workspaces {
		if samePath(w.Path, normalized) {
			return WorkspaceConfig{}, fmt.Errorf("path %q is already workspace %q", normalized, w.ID)
		}
	}
	if id == "" {
		id, err = GenerateWorkspaceID()
		if err != nil {
			return WorkspaceConfig{}, err
		}
	}
	if !validWorkspaceID(id) {
		return WorkspaceConfig{}, errors.New("workspace id must be 1-128 characters of letters, digits, dot, dash, or underscore")
	}
	if _, exists := c.WorkspaceByID(id); exists {
		return WorkspaceConfig{}, fmt.Errorf("workspace id %q already exists", id)
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(normalized)
	}
	if len(name) > 128 {
		return WorkspaceConfig{}, errors.New("workspace name is too long")
	}
	extras, err := NormalizeRoots(extraRoots)
	if err != nil {
		return WorkspaceConfig{}, err
	}
	ws := WorkspaceConfig{
		ID:         id,
		Name:       name,
		Path:       normalized,
		ExtraRoots: DedupeRoots(normalized, extras),
	}
	c.Workspaces = append(c.Workspaces, ws)
	if c.DefaultWorkspaceID == "" {
		c.DefaultWorkspaceID = ws.ID
	}
	return ws, nil
}

// RemoveWorkspace removes a workspace. The last workspace cannot be removed.
func (c *Config) RemoveWorkspace(id string) error {
	if len(c.Workspaces) <= 1 {
		return errors.New("cannot remove the last workspace")
	}
	index := -1
	for i, w := range c.Workspaces {
		if w.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("workspace %q not found", id)
	}
	c.Workspaces = append(c.Workspaces[:index], c.Workspaces[index+1:]...)
	if c.DefaultWorkspaceID == id {
		c.DefaultWorkspaceID = c.Workspaces[0].ID
	}
	return nil
}

// SetDefaultWorkspace selects the workspace used when a call omits workspaceId.
func (c *Config) SetDefaultWorkspace(id string) error {
	if _, ok := c.WorkspaceByID(id); !ok {
		return fmt.Errorf("workspace %q not found", id)
	}
	c.DefaultWorkspaceID = id
	return nil
}

// EffectiveRootsFor returns the workspace path plus its deduplicated extra roots.
func (c *Config) EffectiveRootsFor(id string) ([]string, error) {
	w, ok := c.WorkspaceByID(id)
	if !ok {
		return nil, fmt.Errorf("workspace %q not found", id)
	}
	out := make([]string, 0, 1+len(w.ExtraRoots))
	out = append(out, w.Path)
	out = append(out, DedupeRoots(w.Path, w.ExtraRoots)...)
	return out, nil
}

// GenerateWorkspaceID returns ws_ followed by 16 random hex characters.
func GenerateWorkspaceID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate workspace id: %w", err)
	}
	return "ws_" + hex.EncodeToString(b[:]), nil
}

func validWorkspaceID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// LoadDefault loads the new default config, migrating the legacy
// UserConfigDir location when the new path does not exist yet. The bool
// reports whether a migration was performed.
func LoadDefault() (*Config, string, bool, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, "", false, err
	}
	if _, err := os.Stat(path); err == nil {
		cfg, err := Load(path)
		return cfg, path, false, err
	} else if !os.IsNotExist(err) {
		return nil, path, false, err
	}

	legacy, legacyErr := LegacyDefaultPath()
	if legacyErr == nil {
		if _, err := os.Stat(legacy); err == nil {
			cfg, err := Load(legacy)
			if err != nil {
				return nil, path, false, err
			}
			legacyLog := LegacyDefaultLogPath()
			if cfg.LogFile == "" || (legacyLog != "" && samePath(cfg.LogFile, legacyLog)) {
				cfg.LogFile = DefaultLogPath()
			}
			if err := cfg.Save(path); err != nil {
				return nil, path, false, err
			}
			return cfg, path, true, nil
		} else if !os.IsNotExist(err) {
			return nil, path, false, err
		}
	}
	return nil, path, false, fmt.Errorf("config not found at %s; run termio init", path)
}

func (c *Config) applyDefaults() {
	if c.GatewayURL == "" {
		c.GatewayURL = DefaultGatewayURL
	}
	if c.DeviceName == "" {
		c.DeviceName, _ = os.Hostname()
	}
	if c.MaxConcurrentCalls == 0 {
		c.MaxConcurrentCalls = DefaultMaxConcurrent
	}
	if c.CallTimeoutMS == 0 {
		c.CallTimeoutMS = DefaultCallTimeoutMS
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFile == "" {
		c.LogFile = DefaultLogPath()
	}
}

// Validate checks the config without mutating it.
func (c *Config) Validate() error {
	if !validDeviceID(c.DeviceID) {
		return errors.New("device_id must be 1-128 characters of letters, digits, dot, dash, or underscore")
	}
	if len(c.DeviceName) > 128 {
		return errors.New("device_name is too long")
	}
	if strings.TrimSpace(c.Token) == "" {
		return errors.New("token is required")
	}
	if err := validateGatewayURL(c.GatewayURL); err != nil {
		return err
	}
	if len(c.Workspaces) == 0 {
		return errors.New("at least one workspace is required")
	}
	if len(c.Workspaces) > MaxWorkspaces {
		return fmt.Errorf("too many workspaces (maximum %d)", MaxWorkspaces)
	}
	seen := make(map[string]bool, len(c.Workspaces))
	for _, w := range c.Workspaces {
		if !validWorkspaceID(w.ID) {
			return fmt.Errorf("workspace id %q must be 1-128 characters of letters, digits, dot, dash, or underscore", w.ID)
		}
		if seen[w.ID] {
			return fmt.Errorf("duplicate workspace id %q", w.ID)
		}
		seen[w.ID] = true
		if len(w.Name) > 128 {
			return fmt.Errorf("workspace %q name is too long", w.ID)
		}
		if !filepath.IsAbs(w.Path) {
			return fmt.Errorf("workspace %q path must be absolute", w.ID)
		}
		info, err := os.Stat(w.Path)
		if err != nil {
			return fmt.Errorf("workspace %q: %w", w.ID, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", w.ID)
		}
		for _, root := range w.ExtraRoots {
			if !filepath.IsAbs(root) {
				return fmt.Errorf("workspace %q extra root %q must be absolute", w.ID, root)
			}
			info, err := os.Stat(root)
			if err != nil {
				return fmt.Errorf("workspace %q extra root %q: %w", w.ID, root, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("workspace %q extra root %q is not a directory", w.ID, root)
			}
		}
	}
	if c.DefaultWorkspaceID == "" {
		return errors.New("default_workspace_id is required")
	}
	if !seen[c.DefaultWorkspaceID] {
		return fmt.Errorf("default_workspace_id %q is not in workspaces", c.DefaultWorkspaceID)
	}
	if c.MaxConcurrentCalls < 1 || c.MaxConcurrentCalls > MaxConcurrentCallsLimit {
		return fmt.Errorf("max_concurrent_calls must be between 1 and %d", MaxConcurrentCallsLimit)
	}
	if c.CallTimeoutMS < MinCallTimeoutMS || c.CallTimeoutMS > MaxCallTimeoutMS {
		return fmt.Errorf("call_timeout_ms must be between %d and %d", MinCallTimeoutMS, MaxCallTimeoutMS)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be debug, info, warn, or error")
	}
	if c.LogFile != "" && !filepath.IsAbs(c.LogFile) {
		return errors.New("log_file must be absolute")
	}
	return nil
}

func validDeviceID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

func validateGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gateway_url: %w", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return errors.New("gateway_url must use ws:// or wss://")
	}
	if u.Host == "" {
		return errors.New("gateway_url must include a host")
	}
	if u.Scheme == "ws" && !isLoopbackHost(u.Hostname()) {
		return errors.New("plaintext ws:// is only allowed for loopback; use wss://")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NormalizeWorkspace expands ~, makes the path absolute, resolves symlinks,
// and requires an existing directory.
func NormalizeWorkspace(path string) (string, error) {
	return normalizeDir(path, "workspace")
}

// NormalizeRoots expands ~, makes paths absolute, resolves symlinks, and
// requires each extra root to be an existing directory.
func NormalizeRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		normalized, err := normalizeDir(root, "allowed root")
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

func normalizeDir(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s path is empty", label)
	}
	expanded, err := ExpandHome(path)
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

// DedupeRoots removes roots equal to workspace and duplicates among extras.
func DedupeRoots(workspace string, roots []string) []string {
	seen := make(map[string]bool, len(roots)+1)
	key := func(p string) string {
		p = filepath.Clean(p)
		if runtime.GOOS == "windows" {
			p = strings.ToLower(p)
		}
		return p
	}
	seen[key(workspace)] = true
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		k := key(root)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, root)
	}
	return out
}

// IsBroadWorkspace reports whether path is a filesystem root or the user's
// home directory. The CLI warns about these because they grant broad access.
func IsBroadWorkspace(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if filepath.Dir(abs) == abs {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(abs, home) {
		return true
	}
	return false
}

func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool {
	ca, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		ca = filepath.Clean(a)
	}
	cb, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		cb = filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

// ExpandHome expands a leading ~ or ~/ (or ~\ on Windows) to the user's home.
func ExpandHome(p string) (string, error) {
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

// Save normalizes roots, validates, and atomically writes the config with
// owner-only permissions on platforms that honor them.
func (c *Config) Save(path string) error {
	if err := c.normalizeWorkspaces(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// RedactedToken returns a display-safe token preview.
func RedactedToken(token string) string {
	if len(token) <= 8 {
		if token == "" {
			return ""
		}
		return "***"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
