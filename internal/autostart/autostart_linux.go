//go:build linux

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const linuxUnitName = "termio.service"

type linuxManager struct{}

func newManager() Manager { return &linuxManager{} }

func (m *linuxManager) unitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", linuxUnitName), nil
}

func (m *linuxManager) Install(execPath, configPath string) error {
	if err := validateInstallInputs(execPath, configPath); err != nil {
		return err
	}
	unit, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf(`[Unit]
Description=Termio local agent
After=network-online.target

[Service]
Type=simple
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5
TimeoutStopSec=10

[Install]
WantedBy=default.target
`, systemdQuote(execPath), systemdQuote(configPath))
	if err := os.WriteFile(unit, []byte(content), 0o600); err != nil {
		return err
	}
	if _, err := runCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := runCommand("systemctl", "--user", "enable", "--now", linuxUnitName); err != nil {
		return fmt.Errorf("%w (a user systemd session is required; for headless boot run: loginctl enable-linger $USER)", err)
	}
	return nil
}

func (m *linuxManager) Uninstall() error {
	runIgnore("systemctl", "--user", "disable", "--now", linuxUnitName)
	unit, err := m.unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
		return err
	}
	runIgnore("systemctl", "--user", "daemon-reload")
	return nil
}

func (m *linuxManager) Start() error {
	if _, err := runCommand("systemctl", "--user", "start", linuxUnitName); err != nil {
		return err
	}
	return nil
}

func (m *linuxManager) Stop() error {
	if _, err := runCommand("systemctl", "--user", "stop", linuxUnitName); err != nil {
		return err
	}
	return nil
}

func (m *linuxManager) Status() (State, error) {
	unit, err := m.unitPath()
	if err != nil {
		return State{}, err
	}
	_, statErr := os.Stat(unit)
	out, activeErr := runCommand("systemctl", "--user", "is-active", linuxUnitName)
	running := activeErr == nil && strings.TrimSpace(out) == "active"
	return State{
		Installed: statErr == nil,
		Running:   running,
		Detail:    strings.TrimSpace(out),
	}, nil
}

// systemdQuote quotes one ExecStart argument and escapes systemd's % and $
// specifier characters.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}
