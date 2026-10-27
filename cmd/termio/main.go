// Command termio is the local Termio connector.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"termio/internal/autostart"
	"termio/internal/config"
	"termio/internal/daemon"
	"termio/internal/logging"
	"termio/internal/tools"
	"termio/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "termio:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return errors.New("a command is required")
	}
	switch args[0] {
	case "init":
		return runInit(args[1:])
	case "run":
		return runDaemon(args[1:])
	case "install":
		return runInstall(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "start":
		return runControl("start", args[1:])
	case "stop":
		return runControl("stop", args[1:])
	case "status":
		return runStatus(args[1:])
	case "workspace":
		return runWorkspace(args[1:])
	case "version", "--version", "-version":
		printVersion()
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path (default: per-user config directory)")
	url := fs.String("url", config.DefaultGatewayURL, "gateway WebSocket URL")
	token := fs.String("token", "", "device bearer token (required)")
	deviceID := fs.String("device-id", "", "device id (default: generated)")
	deviceName := fs.String("device-name", "", "human-readable device name")
	logFile := fs.String("log-file", "", "log file path (default: per-user cache directory)")
	logLevel := fs.String("log-level", "info", "debug, info, warn, or error")
	workspaceFlag := fs.String("workspace", "", "workspace directory (default: ~/LARES; must exist)")
	force := fs.Bool("force", false, "overwrite an existing config")
	var roots stringList
	fs.Var(&roots, "root", "extra allowed root directory (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if strings.TrimSpace(*token) == "" {
		return errors.New("--token is required; use the token configured on the gateway")
	}
	path, err := resolvePath(*configFlag)
	if err != nil {
		return err
	}
	if !*force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (use --force to overwrite)", path)
		}
	}
	cfg, err := config.NewDefault()
	if err != nil {
		return err
	}
	cfg.GatewayURL = *url
	cfg.Token = *token
	if *deviceID != "" {
		cfg.DeviceID = *deviceID
	}
	if *deviceName != "" {
		cfg.DeviceName = *deviceName
	}
	if *logFile != "" {
		abs, err := filepath.Abs(*logFile)
		if err != nil {
			return err
		}
		cfg.LogFile = abs
	}
	cfg.LogLevel = *logLevel

	var workspace string
	var extras []string
	switch {
	case *workspaceFlag != "":
		expanded, err := config.ExpandHome(*workspaceFlag)
		if err != nil {
			return err
		}
		info, err := os.Stat(expanded)
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace %q is not a directory", *workspaceFlag)
		}
		workspace = expanded
		extras = roots
	case len(roots) > 0:
		// Backward compatibility: `init --root X` selects X as the workspace
		// and treats any additional roots as extras.
		expanded, err := config.ExpandHome(roots[0])
		if err != nil {
			return err
		}
		workspace = expanded
		extras = roots[1:]
	default:
		workspace, err = config.DefaultWorkspacePath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			return fmt.Errorf("create default workspace %s: %w", workspace, err)
		}
	}
	cfg.Workspace = workspace
	cfg.AllowedRoots = extras
	if config.IsBroadWorkspace(workspace) {
		fmt.Fprintf(os.Stderr, "warning: workspace %s is a broad directory (home or filesystem root)\n", workspace)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}

	fmt.Printf("Config:     %s\n", path)
	fmt.Printf("Device ID:  %s\n", cfg.DeviceID)
	fmt.Printf("Gateway:    %s\n", cfg.GatewayURL)
	fmt.Printf("Workspace:  %s\n", cfg.Workspace)
	fmt.Printf("Log file:   %s\n", cfg.LogFile)
	if len(cfg.AllowedRoots) == 0 {
		fmt.Printf("Extra roots: none\n")
	} else {
		fmt.Printf("Extra roots:\n")
		for _, root := range cfg.AllowedRoots {
			fmt.Printf("  - %s\n", root)
		}
	}
	fmt.Printf("\nStart:      termio run --config \"%s\"\n", path)
	fmt.Printf("Autostart:  termio install --config \"%s\"\n", path)
	return nil
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	log, closer, err := logging.New(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return err
	}
	defer closer.Close()

	sb, err := tools.NewSandbox(cfg.Workspace, cfg.AllowedRoots)
	if err != nil {
		return err
	}
	defer sb.Close()
	registry := tools.DefaultRegistry(sb)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("termio starting",
		"version", version.Version,
		"device_id", cfg.DeviceID,
		"gateway", cfg.GatewayURL,
		"config", path,
		"workspace", cfg.Workspace,
		"roots", sb.Roots(),
		"tools", len(registry.Specs()))

	d := daemon.New(cfg, log, registry)
	if err := d.Run(ctx); err != nil {
		return err
	}
	log.Info("termio stopped")
	return nil
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	_, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	execPath, err := serviceExecutable()
	if err != nil {
		return err
	}
	if err := autostart.New().Install(execPath, path); err != nil {
		return err
	}
	fmt.Printf("Autostart installed for %s\n", execPath)
	fmt.Printf("Status: termio status\n")
	if runtime.GOOS == "darwin" {
		fmt.Println("Note: grant Full Disk Access to this binary in System Settings > Privacy & Security if you search protected folders.")
	}
	if runtime.GOOS == "linux" {
		fmt.Println("Note: for headless boot, run: loginctl enable-linger $USER")
	}
	if runtime.GOOS == "windows" && !strings.Contains(strings.ToLower(filepath.Base(execPath)), "termio-agent") {
		fmt.Println("Note: a termio-agent.exe build was not found next to this binary; autostart may show a console window.")
	}
	return nil
}

func runUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if err := autostart.New().Uninstall(); err != nil {
		return err
	}
	fmt.Println("Autostart removed. Config and log files were left in place.")
	return nil
}

func runControl(action string, args []string) error {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	m := autostart.New()
	var err error
	if action == "start" {
		err = m.Start()
	} else {
		err = m.Stop()
	}
	if err != nil {
		return err
	}
	fmt.Printf("Autostart service %s requested.\n", action)
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	state, err := autostart.New().Status()
	if err != nil {
		return err
	}
	fmt.Printf("Installed: %t\n", state.Installed)
	fmt.Printf("Running:   %t\n", state.Running)
	if state.Detail != "" {
		fmt.Printf("Detail:    %s\n", state.Detail)
	}
	return nil
}

func runWorkspace(args []string) error {
	if len(args) == 0 {
		return errors.New("workspace requires show, set, or reset")
	}
	switch args[0] {
	case "show":
		return runWorkspaceShow(args[1:])
	case "set":
		return runWorkspaceSet(args[1:])
	case "reset":
		return runWorkspaceReset(args[1:])
	default:
		return fmt.Errorf("unknown workspace command %q (expected show, set, or reset)", args[0])
	}
}

func runWorkspaceShow(args []string) error {
	fs := flag.NewFlagSet("workspace show", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	source := "selected"
	if cfg.IsDefaultWorkspace() {
		source = "default"
	}
	exists := false
	readable := false
	if info, err := os.Stat(cfg.Workspace); err == nil && info.IsDir() {
		exists = true
		if f, err := os.Open(cfg.Workspace); err == nil {
			readable = true
			f.Close()
		}
	}
	fmt.Printf("Config:     %s\n", path)
	fmt.Printf("Workspace:  %s\n", cfg.Workspace)
	fmt.Printf("Source:     %s\n", source)
	fmt.Printf("Exists:     %t\n", exists)
	fmt.Printf("Readable:   %t\n", readable)
	fmt.Printf("Effective roots:\n")
	for _, root := range cfg.EffectiveRoots() {
		fmt.Printf("  - %s\n", root)
	}
	return nil
}

func runWorkspaceSet(args []string) error {
	fs := flag.NewFlagSet("workspace set", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: termio workspace set [--config <path>] <directory>")
	}
	expanded, err := config.ExpandHome(fs.Arg(0))
	if err != nil {
		return err
	}
	target, err := filepath.Abs(expanded)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", target)
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	cfg.Workspace = target
	if config.IsBroadWorkspace(target) {
		fmt.Fprintf(os.Stderr, "warning: workspace %s is a broad directory (home or filesystem root)\n", target)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Workspace:  %s\n", cfg.Workspace)
	fmt.Println("Restart the daemon for the new workspace to take effect.")
	return nil
}

func runWorkspaceReset(args []string) error {
	fs := flag.NewFlagSet("workspace reset", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	workspace, err := config.DefaultWorkspacePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return fmt.Errorf("create default workspace %s: %w", workspace, err)
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	cfg.Workspace = workspace
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Workspace:  %s\n", cfg.Workspace)
	fmt.Println("Restart the daemon for the new workspace to take effect.")
	return nil
}

func loadConfig(flagValue string) (*config.Config, string, error) {
	path, err := resolvePath(flagValue)
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, path, fmt.Errorf("config not found at %s; run `termio init --token ...` first", path)
		}
		return nil, path, err
	}
	return cfg, path, nil
}

func resolvePath(flagValue string) (string, error) {
	if flagValue == "" {
		path, err := config.DefaultPath()
		if err != nil {
			return "", err
		}
		flagValue = path
	}
	abs, err := filepath.Abs(flagValue)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func serviceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if runtime.GOOS == "windows" {
		dir := filepath.Dir(exe)
		candidates := []string{
			filepath.Join(dir, "termio-agent.exe"),
			filepath.Join(dir, fmt.Sprintf("termio-agent-%s-%s.exe", runtime.GOOS, runtime.GOARCH)),
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return exe, nil
}

func printVersion() {
	fmt.Printf("termio %s (%s, built %s)\n", version.Version, version.Commit, version.BuildTime)
	fmt.Printf("%s/%s %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func printUsage() {
	fmt.Print(`Termio local agent

Usage:
  termio init --token <token> [--workspace <dir>] [--root <extra-dir>]... [--url <ws-url>] [--log-file <path>] [--log-level <level>] [--config <path>]
  termio run [--config <path>]
  termio workspace show [--config <path>]
  termio workspace set [--config <path>] <directory>
  termio workspace reset [--config <path>]
  termio install [--config <path>]
  termio uninstall
  termio start
  termio stop
  termio status
  termio version

Notes:
  - The gateway URL must use wss:// unless it is loopback (ws://127.0.0.1).
  - The default workspace is ~/LARES. --workspace selects an existing directory.
  - --root adds extra allowed roots; tools can only access the workspace and extras.
  - With only --root and no --workspace, the first root becomes the workspace (compatibility).
  - The config default is the per-user config directory (termio/config.json).
`)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
