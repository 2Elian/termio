// Package autostart installs the daemon as a per-user login service. Per-user
// scope is deliberate: system services (Windows LocalSystem, Linux root) cannot
// reliably read the invoking user's files.
package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// State describes the current installation and process state.
type State struct {
	Installed bool
	Running   bool
	Detail    string
}

// Manager controls a platform-specific per-user autostart entry.
type Manager interface {
	Install(execPath, configPath string) error
	Uninstall() error
	Start() error
	Stop() error
	Status() (State, error)
}

// New returns the manager for the current platform.
func New() Manager { return newManager() }

func validateInstallInputs(execPath, configPath string) error {
	if !filepath.IsAbs(execPath) {
		return fmt.Errorf("executable path must be absolute: %q", execPath)
	}
	if !filepath.IsAbs(configPath) {
		return fmt.Errorf("config path must be absolute: %q", configPath)
	}
	info, err := os.Stat(execPath)
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("executable path is a directory: %q", execPath)
	}
	info, err = os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("config path is a directory: %q", configPath)
	}
	return nil
}

func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

func runIgnore(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out))
}
