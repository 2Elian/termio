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
	DefaultWorkspaceDirName = "LARES"
)

// Config is the on-disk daemon configuration.
type Config struct {
	GatewayURL         string   `json:"gateway_url"`
	DeviceID           string   `json:"device_id"`
	DeviceName         string   `json:"device_name"`
	Token              string   `json:"token"`
	Workspace          string   `json:"workspace"`
	AllowedRoots       []string `json:"allowed_roots"`
	MaxConcurrentCalls int      `json:"max_concurrent_calls"`
	CallTimeoutMS      int      `json:"call_timeout_ms"`
	LogLevel           string   `json:"log_level"`
	LogFile            string   `json:"log_file"`
}

// DefaultWorkspacePath returns ~/LARES (or %USERPROFILE%\LARES).
func DefaultWorkspacePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, DefaultWorkspaceDirName), nil
}

// DefaultPath returns the per-user config path.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "termio", "config.json"), nil
}

// DefaultLogPath returns the per-user log path.
func DefaultLogPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "termio", "termio.log")
}

// NewDefault builds a config with a fresh device ID, the default LARES
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
		GatewayURL:         DefaultGatewayURL,
		DeviceID:           id,
		DeviceName:         host,
		Workspace:          workspace,
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
	if err := c.migrateWorkspace(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// migrateWorkspace supports configs written before the workspace concept:
// the first allowed root becomes the workspace and the rest stay as extras.
// It also normalizes both lists and removes duplicates.
func (c *Config) migrateWorkspace() error {
	if c.Workspace == "" {
		if len(c.AllowedRoots) > 0 {
			c.Workspace = c.AllowedRoots[0]
			c.AllowedRoots = append([]string(nil), c.AllowedRoots[1:]...)
		} else {
			ws, err := DefaultWorkspacePath()
			if err != nil {
				return err
			}
			c.Workspace = ws
		}
	}
	workspace, err := NormalizeWorkspace(c.Workspace)
	if err != nil {
		return fmt.Errorf("workspace: %w (run termio init or set a valid workspace)", err)
	}
	c.Workspace = workspace
	roots, err := NormalizeRoots(c.AllowedRoots)
	if err != nil {
		return fmt.Errorf("allowed_roots: %w", err)
	}
	c.AllowedRoots = DedupeRoots(c.Workspace, roots)
	return nil
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
	if strings.TrimSpace(c.Workspace) == "" {
		return errors.New("workspace is required")
	}
	if !filepath.IsAbs(c.Workspace) {
		return fmt.Errorf("workspace %q must be absolute", c.Workspace)
	}
	info, err := os.Stat(c.Workspace)
	if err != nil {
		return fmt.Errorf("workspace %q: %w", c.Workspace, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", c.Workspace)
	}
	for _, root := range c.AllowedRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("allowed root %q must be absolute", root)
		}
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("allowed root %q: %w", root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("allowed root %q is not a directory", root)
		}
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

// EffectiveRoots returns the workspace followed by deduplicated extra roots.
func (c *Config) EffectiveRoots() []string {
	out := make([]string, 0, 1+len(c.AllowedRoots))
	out = append(out, c.Workspace)
	out = append(out, DedupeRoots(c.Workspace, c.AllowedRoots)...)
	return out
}

// IsDefaultWorkspace reports whether the config uses the default ~/LARES path.
func (c *Config) IsDefaultWorkspace() bool {
	def, err := DefaultWorkspacePath()
	if err != nil {
		return false
	}
	return samePath(c.Workspace, def)
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
	workspace, err := NormalizeWorkspace(c.Workspace)
	if err != nil {
		return err
	}
	c.Workspace = workspace
	roots, err := NormalizeRoots(c.AllowedRoots)
	if err != nil {
		return err
	}
	c.AllowedRoots = DedupeRoots(c.Workspace, roots)
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
