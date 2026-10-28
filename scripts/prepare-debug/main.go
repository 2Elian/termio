// Command prepare-debug writes a ready-to-debug .debug/config.json with two
// workspaces and a fixture file. It is invoked by the VS Code preLaunchTask so
// F5 always starts a realistic multi-workspace environment.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"termio/internal/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "prepare-debug:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("prepare-debug", flag.ContinueOnError)
	configPath := fs.String("config", filepath.Join(".debug", "config.json"), "config output path")
	root := fs.String("workspace", ".", "default workspace directory")
	extra := fs.String("extra-workspace", filepath.Join(".debug", "ws-docs"), "second workspace directory")
	extraID := fs.String("extra-workspace-id", "ws_docs", "second workspace id")
	extraName := fs.String("extra-workspace-name", "docs", "second workspace display name")
	url := fs.String("url", config.DefaultGatewayURL, "gateway WebSocket URL")
	token := fs.String("token", "dev-token", "device bearer token")
	deviceID := fs.String("device-id", "dev_debug", "device id")
	deviceName := fs.String("device-name", "debug-device", "device display name")
	logFile := fs.String("log-file", filepath.Join(".debug", "termio.log"), "log file path")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	rootPath, err := filepath.Abs(*root)
	if err != nil {
		return err
	}
	extraPath, err := filepath.Abs(*extra)
	if err != nil {
		return err
	}
	configPathAbs, err := filepath.Abs(*configPath)
	if err != nil {
		return err
	}
	logPath, err := filepath.Abs(*logFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(extraPath, 0o700); err != nil {
		return fmt.Errorf("create debug workspace: %w", err)
	}

	cfg := &config.Config{
		GatewayURL: *url,
		DeviceID:   *deviceID,
		DeviceName: *deviceName,
		Token:      *token,
		Workspaces: []config.WorkspaceConfig{
			{ID: config.DefaultWorkspaceID, Name: "default", Path: rootPath},
			{ID: *extraID, Name: *extraName, Path: extraPath},
		},
		DefaultWorkspaceID: config.DefaultWorkspaceID,
		MaxConcurrentCalls: config.DefaultMaxConcurrent,
		CallTimeoutMS:      config.DefaultCallTimeoutMS,
		LogLevel:           "debug",
		LogFile:            logPath,
	}
	if err := cfg.Save(configPathAbs); err != nil {
		return err
	}

	fixture := filepath.Join(extraPath, "notes.md")
	if _, err := os.Stat(fixture); os.IsNotExist(err) {
		if err := os.WriteFile(fixture, []byte("hello from docs workspace\n"), 0o600); err != nil {
			return fmt.Errorf("write fixture: %w", err)
		}
	}

	ws, _ := cfg.WorkspaceByID(*extraID)
	fmt.Printf("Debug config: %s\n", configPathAbs)
	fmt.Printf("Default:      %s -> %s\n", config.DefaultWorkspaceID, rootPath)
	fmt.Printf("Second:       %s -> %s\n", ws.ID, ws.Path)
	fmt.Printf("Fixture:      %s\n", fixture)
	return nil
}
