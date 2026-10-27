//go:build windows

package autostart

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	windowsTaskName = "TermioAgent"
	windowsRunKey   = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
)

type windowsManager struct{}

func newManager() Manager { return &windowsManager{} }

func (m *windowsManager) Install(execPath, configPath string) error {
	if err := validateInstallInputs(execPath, configPath); err != nil {
		return err
	}
	cmdLine := winQuote(execPath) + " run --config " + winQuote(configPath)

	taskErr := error(nil)
	if _, err := runCommand("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", windowsTaskName, "/TR", cmdLine); err == nil {
		return nil
	} else {
		taskErr = err
	}

	// Fallback for non-elevated installs. The HKCU Run entry starts at logon
	// and needs no administrator rights.
	if _, err := runCommand("reg", "add", windowsRunKey, "/v", windowsTaskName, "/t", "REG_SZ", "/d", cmdLine, "/f"); err != nil {
		return fmt.Errorf("scheduled task failed: %v; registry fallback failed: %w", taskErr, err)
	}
	return nil
}

func (m *windowsManager) Uninstall() error {
	runIgnore("schtasks", "/End", "/TN", windowsTaskName)
	runIgnore("schtasks", "/Delete", "/F", "/TN", windowsTaskName)
	runIgnore("reg", "delete", windowsRunKey, "/v", windowsTaskName, "/f")
	return nil
}

func (m *windowsManager) Start() error {
	if _, err := runCommand("schtasks", "/Run", "/TN", windowsTaskName); err != nil {
		return fmt.Errorf("%w (if install used the HKCU fallback, sign out and back in instead)", err)
	}
	return nil
}

func (m *windowsManager) Stop() error {
	if _, err := runCommand("schtasks", "/End", "/TN", windowsTaskName); err != nil {
		return err
	}
	return nil
}

func (m *windowsManager) Status() (State, error) {
	taskInstalled := commandSucceeds("schtasks", "/Query", "/TN", windowsTaskName, "/FO", "LIST")
	regInstalled := commandSucceeds("reg", "query", windowsRunKey, "/v", windowsTaskName)
	running := processRunning("termio-agent.exe", 0) || processRunning("termio.exe", os.Getpid())

	detail := "not installed"
	switch {
	case taskInstalled:
		detail = "scheduled task"
	case regInstalled:
		detail = "HKCU Run entry"
	}
	return State{
		Installed: taskInstalled || regInstalled,
		Running:   running,
		Detail:    detail,
	}, nil
}

func commandSucceeds(name string, args ...string) bool {
	_, err := runCommand(name, args...)
	return err == nil
}

func processRunning(image string, excludePID int) bool {
	out := runIgnore("tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH")
	reader := csv.NewReader(strings.NewReader(out))
	reader.FieldsPerRecord = -1
	for {
		record, err := reader.Read()
		if err != nil {
			return false
		}
		if len(record) < 2 || !strings.EqualFold(strings.TrimSpace(record[0]), image) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(record[1]))
		if err != nil {
			continue
		}
		if pid != excludePID {
			return true
		}
	}
}

func winQuote(s string) string {
	return `"` + s + `"`
}
