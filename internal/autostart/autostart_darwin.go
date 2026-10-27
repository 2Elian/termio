//go:build darwin

package autostart

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const darwinLabel = "com.termio.agent"

type darwinManager struct{}

func newManager() Manager { return &darwinManager{} }

func (m *darwinManager) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist"), nil
}

func (m *darwinManager) logPaths() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, "Library", "Logs", "termio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "termio.out.log"), filepath.Join(dir, "termio.err.log"), nil
}

func (m *darwinManager) Install(execPath, configPath string) error {
	if err := validateInstallInputs(execPath, configPath); err != nil {
		return err
	}
	plist, err := m.plistPath()
	if err != nil {
		return err
	}
	stdout, stderr, err := m.logPaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		return err
	}
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>--config</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, darwinLabel, xmlEscape(execPath), xmlEscape(configPath), xmlEscape(stdout), xmlEscape(stderr))
	if err := os.WriteFile(plist, []byte(content), 0o600); err != nil {
		return err
	}

	domain := fmt.Sprintf("gui/%d", os.Getuid())
	runIgnore("launchctl", "bootout", domain+"/"+darwinLabel)
	if _, err := runCommand("launchctl", "bootstrap", domain, plist); err != nil {
		return fmt.Errorf("%w (launchd agents require an active GUI login session)", err)
	}
	return nil
}

func (m *darwinManager) Uninstall() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	runIgnore("launchctl", "bootout", domain+"/"+darwinLabel)
	plist, err := m.plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *darwinManager) Start() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if _, err := runCommand("launchctl", "kickstart", domain+"/"+darwinLabel); err == nil {
		return nil
	}
	plist, err := m.plistPath()
	if err != nil {
		return err
	}
	if _, err := runCommand("launchctl", "bootstrap", domain, plist); err != nil {
		return err
	}
	return nil
}

func (m *darwinManager) Stop() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, err := runCommand("launchctl", "kill", "SIGTERM", domain+"/"+darwinLabel)
	return err
}

func (m *darwinManager) Status() (State, error) {
	plist, err := m.plistPath()
	if err != nil {
		return State{}, err
	}
	_, statErr := os.Stat(plist)
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_, printErr := runCommand("launchctl", "print", domain+"/"+darwinLabel)
	detail := "not loaded"
	if printErr == nil {
		detail = "loaded"
	}
	return State{
		Installed: statErr == nil,
		Running:   printErr == nil,
		Detail:    detail,
	}, nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
