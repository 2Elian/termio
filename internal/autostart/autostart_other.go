//go:build !linux && !darwin && !windows

package autostart

import (
	"errors"
	"runtime"
)

type unsupportedManager struct{}

func newManager() Manager { return &unsupportedManager{} }

func (m *unsupportedManager) Install(string, string) error {
	return errors.New("autostart is not supported on " + runtime.GOOS)
}

func (m *unsupportedManager) Uninstall() error {
	return errors.New("autostart is not supported on " + runtime.GOOS)
}
func (m *unsupportedManager) Start() error {
	return errors.New("autostart is not supported on " + runtime.GOOS)
}
func (m *unsupportedManager) Stop() error {
	return errors.New("autostart is not supported on " + runtime.GOOS)
}

func (m *unsupportedManager) Status() (State, error) {
	return State{Detail: "unsupported platform: " + runtime.GOOS}, nil
}
