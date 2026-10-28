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
	configFlag := fs.String("config", "", "config path (default: $LARES/.termio/config.json)")
	url := fs.String("url", config.DefaultGatewayURL, "gateway WebSocket URL")
	token := fs.String("token", "", "device bearer token (required)")
	deviceID := fs.String("device-id", "", "device id (default: generated)")
	deviceName := fs.String("device-name", "", "human-readable device name")
	logFile := fs.String("log-file", "", "log file path (default: $LARES/.termio/termio.log)")
	logLevel := fs.String("log-level", "info", "debug, info, warn, or error")
	workspaceFlag := fs.String("workspace", "", "workspace directory (default: ~/.lares; must exist)")
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
	cfg.Workspaces = []config.WorkspaceConfig{{
		ID:         config.DefaultWorkspaceID,
		Name:       "default",
		Path:       workspace,
		ExtraRoots: extras,
	}}
	cfg.DefaultWorkspaceID = config.DefaultWorkspaceID
	if config.IsBroadWorkspace(workspace) {
		fmt.Fprintf(os.Stderr, "warning: workspace %s is a broad directory (home or filesystem root)\n", workspace)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}

	fmt.Printf("Config:     %s\n", path)
	fmt.Printf("Device ID:  %s\n", cfg.DeviceID)
	fmt.Printf("Gateway:    %s\n", cfg.GatewayURL)
	fmt.Printf("Workspace:  %s (%s)\n", cfg.Workspaces[0].ID, cfg.Workspaces[0].Path)
	fmt.Printf("Log file:   %s\n", cfg.LogFile)
	if len(cfg.Workspaces[0].ExtraRoots) == 0 {
		fmt.Printf("Extra roots: none\n")
	} else {
		fmt.Printf("Extra roots:\n")
		for _, root := range cfg.Workspaces[0].ExtraRoots {
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

	workspaces, err := daemon.LoadWorkspaces(cfg, path, nil)
	if err != nil {
		return err
	}
	defer workspaces.Close()
	defaultWorkspace := workspaces.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("termio starting",
		"version", version.Version,
		"device_id", cfg.DeviceID,
		"gateway", cfg.GatewayURL,
		"config", path,
		"default_workspace", workspaces.DefaultID(),
		"workspace", defaultWorkspace.Sandbox.Workspace(),
		"roots", defaultWorkspace.Sandbox.Roots(),
		"workspaces", len(workspaces.List()),
		"tools", len(defaultWorkspace.Registry.Specs()))
	for _, info := range workspaces.List() {
		ws, _ := workspaces.Get(info.ID)
		log.Debug("workspace loaded",
			"workspace_id", info.ID,
			"name", info.Name,
			"path", ws.Sandbox.Workspace(),
			"roots", ws.Sandbox.Roots())
	}

	d := daemon.New(cfg, log, workspaces)
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
		return errors.New("workspace requires list, show, add, remove, default, set, or reset")
	}
	switch args[0] {
	case "list":
		return runWorkspaceList(args[1:])
	case "show":
		return runWorkspaceShow(args[1:])
	case "add":
		return runWorkspaceAdd(args[1:])
	case "remove":
		return runWorkspaceRemove(args[1:])
	case "default":
		return runWorkspaceDefault(args[1:])
	case "set":
		return runWorkspaceSet(args[1:])
	case "reset":
		return runWorkspaceReset(args[1:])
	default:
		return fmt.Errorf("unknown workspace command %q (expected list, show, add, remove, default, set, or reset)", args[0])
	}
}

func runWorkspaceList(args []string) error {
	fs := flag.NewFlagSet("workspace list", flag.ContinueOnError)
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
	fmt.Printf("Config: %s\n", path)
	for _, w := range cfg.Workspaces {
		marker := " "
		if w.ID == cfg.DefaultWorkspaceID {
			marker = "*"
		}
		fmt.Printf("%s %-24s %-20s %s\n", marker, w.ID, w.Name, w.Path)
		if len(w.ExtraRoots) > 0 {
			fmt.Printf("    extra roots: %s\n", strings.Join(w.ExtraRoots, ", "))
		}
	}
	fmt.Println("\n* = default workspace (used when a call omits termio/workspaceId)")
	return nil
}

func runWorkspaceShow(args []string) error {
	fs := flag.NewFlagSet("workspace show", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("usage: termio workspace show [--config <path>] [workspace-id]")
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	id := cfg.DefaultWorkspaceID
	if fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	w, ok := cfg.WorkspaceByID(id)
	if !ok {
		return fmt.Errorf("workspace %q not found; run termio workspace list", id)
	}
	source := "registered"
	if id == cfg.DefaultWorkspaceID {
		source = "default"
	}
	exists := false
	readable := false
	if info, err := os.Stat(w.Path); err == nil && info.IsDir() {
		exists = true
		if f, err := os.Open(w.Path); err == nil {
			readable = true
			f.Close()
		}
	}
	roots, err := cfg.EffectiveRootsFor(id)
	if err != nil {
		return err
	}
	fmt.Printf("Config:     %s\n", path)
	fmt.Printf("ID:         %s\n", w.ID)
	fmt.Printf("Name:       %s\n", w.Name)
	fmt.Printf("Workspace:  %s\n", w.Path)
	fmt.Printf("Source:     %s\n", source)
	fmt.Printf("Exists:     %t\n", exists)
	fmt.Printf("Readable:   %t\n", readable)
	fmt.Printf("Effective roots:\n")
	for _, root := range roots {
		fmt.Printf("  - %s\n", root)
	}
	return nil
}

func runWorkspaceAdd(args []string) error {
	fs := flag.NewFlagSet("workspace add", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	idFlag := fs.String("id", "", "workspace id (default: generated)")
	nameFlag := fs.String("name", "", "display name (default: directory name)")
	var roots stringList
	fs.Var(&roots, "root", "extra allowed root directory (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: termio workspace add [--config <path>] [--id <id>] [--name <name>] [--root <dir>]... <directory>")
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	ws, err := cfg.AddWorkspace(fs.Arg(0), *nameFlag, *idFlag, roots)
	if err != nil {
		return err
	}
	if config.IsBroadWorkspace(ws.Path) {
		fmt.Fprintf(os.Stderr, "warning: workspace %s is a broad directory (home or filesystem root)\n", ws.Path)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Workspace:  %s (%s)\n", ws.ID, ws.Path)
	fmt.Println("Restart the daemon so it opens the new workspace.")
	return nil
}

func runWorkspaceRemove(args []string) error {
	fs := flag.NewFlagSet("workspace remove", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: termio workspace remove [--config <path>] <workspace-id>")
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	if err := cfg.RemoveWorkspace(fs.Arg(0)); err != nil {
		return err
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Removed workspace %s. Files on disk were not touched.\n", fs.Arg(0))
	fmt.Println("Restart the daemon so it stops serving the removed workspace.")
	return nil
}

func runWorkspaceDefault(args []string) error {
	fs := flag.NewFlagSet("workspace default", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: termio workspace default [--config <path>] <workspace-id>")
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	if err := cfg.SetDefaultWorkspace(fs.Arg(0)); err != nil {
		return err
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Default workspace: %s\n", cfg.DefaultWorkspaceID)
	fmt.Println("The default is used when a call omits termio/workspaceId; restart the daemon to apply.")
	return nil
}

// runWorkspaceSet keeps the old single-workspace command working: it selects
// an existing workspace by path or registers the directory, then makes it the
// default. Other registered workspaces stay available.
func runWorkspaceSet(args []string) error {
	fs := flag.NewFlagSet("workspace set", flag.ContinueOnError)
	configFlag := fs.String("config", "", "config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: termio workspace set [--config <path>] <directory>")
	}
	target := fs.Arg(0)
	normalized, err := config.NormalizeWorkspace(target)
	if err != nil {
		return err
	}
	cfg, path, err := loadConfig(*configFlag)
	if err != nil {
		return err
	}
	ws, ok := cfg.FindWorkspaceByPath(normalized)
	if !ok {
		ws, err = cfg.AddWorkspace(normalized, "", "", nil)
		if err != nil {
			return err
		}
	}
	if err := cfg.SetDefaultWorkspace(ws.ID); err != nil {
		return err
	}
	if config.IsBroadWorkspace(ws.Path) {
		fmt.Fprintf(os.Stderr, "warning: workspace %s is a broad directory (home or filesystem root)\n", ws.Path)
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Default workspace: %s (%s)\n", ws.ID, ws.Path)
	fmt.Println("Restart the daemon if this workspace was newly added.")
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
	// If another registered workspace already points at the app home, reuse it
	// rather than creating a duplicate path entry.
	if existing, ok := cfg.FindWorkspaceByPath(workspace); ok {
		if err := cfg.SetDefaultWorkspace(existing.ID); err != nil {
			return err
		}
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Printf("Default workspace: %s (%s)\n", existing.ID, existing.Path)
		fmt.Println("Restart the daemon to apply the reset.")
		return nil
	}
	if ws, ok := cfg.WorkspaceByID(config.DefaultWorkspaceID); ok {
		ws.Path = workspace
		for i := range cfg.Workspaces {
			if cfg.Workspaces[i].ID == ws.ID {
				cfg.Workspaces[i] = ws
			}
		}
	} else if _, err := cfg.AddWorkspace(workspace, "default", config.DefaultWorkspaceID, nil); err != nil {
		return err
	}
	if err := cfg.SetDefaultWorkspace(config.DefaultWorkspaceID); err != nil {
		return err
	}
	if err := cfg.Save(path); err != nil {
		return err
	}
	fmt.Printf("Default workspace: %s (%s)\n", config.DefaultWorkspaceID, workspace)
	fmt.Println("Restart the daemon to apply the reset.")
	return nil
}

func loadConfig(flagValue string) (*config.Config, string, error) {
	if flagValue == "" {
		cfg, path, migrated, err := config.LoadDefault()
		if err != nil {
			return nil, path, err
		}
		if migrated {
			fmt.Fprintf(os.Stderr, "Migrated config to %s\n", path)
		}
		return cfg, path, nil
	}
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
  termio workspace list [--config <path>]
  termio workspace show [--config <path>] [workspace-id]
  termio workspace add [--config <path>] [--id <id>] [--name <name>] [--root <dir>]... <directory>
  termio workspace remove [--config <path>] <workspace-id>
  termio workspace default [--config <path>] <workspace-id>
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
  - The default workspace is ~/.lares. --workspace selects an existing directory.
  - --root adds extra allowed roots; tools can only access a workspace and its extras.
  - With only --root and no --workspace, the first root becomes the workspace (compatibility).
  - A workspace can serve many sessions; calls are routed by termio/workspaceId.
  - Adding or removing a workspace requires a daemon restart; switching between
    registered workspaces does not.
  - The config default is $LARES/.termio/config.json.
`)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}
