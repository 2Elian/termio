// Command termioctl is a small interactive JSON client for local self-testing.
// It talks to the gateway HTTP API, which forwards tool calls to the real daemon
// over WebSocket. It is a development tool, not part of the device protocol.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultGatewayURL = "http://127.0.0.1:8787"
	defaultTimeoutMS  = 30000
	methodToolsCall   = "tools/call"
)

type client struct {
	baseURL     string
	token       string
	http        *http.Client
	workspaceID string
	sessionID   string
}

type workspaceInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type deviceInfo struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	OS                 string          `json:"os"`
	Arch               string          `json:"arch"`
	Version            string          `json:"version"`
	Workspaces         []workspaceInfo `json:"workspaces"`
	DefaultWorkspaceID string          `json:"default_workspace_id"`
}

type devicesResponse struct {
	Devices []deviceInfo `json:"devices"`
}

// wireCall accepts both the gateway's simple body shape and a JSON-RPC
// tools/call envelope, so users can paste either into the REPL.
type wireCall struct {
	JSONRPC     string          `json:"jsonrpc,omitempty"`
	ID          json.RawMessage `json:"id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Params      *wireCall       `json:"params,omitempty"`
	Name        string          `json:"name,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	TimeoutMS   int             `json:"timeout_ms,omitempty"`
	CallID      string          `json:"call_id,omitempty"`
	WorkspaceID string          `json:"workspace_id,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Meta        *wireMeta       `json:"_meta,omitempty"`
}

type wireMeta struct {
	CallID      string `json:"termio/callId,omitempty"`
	TimeoutMS   int    `json:"termio/timeoutMs,omitempty"`
	WorkspaceID string `json:"termio/workspaceId,omitempty"`
	SessionID   string `json:"termio/sessionId,omitempty"`
}

type normalizedCall struct {
	Name        string
	Arguments   json.RawMessage
	TimeoutMS   int
	CallID      string
	WorkspaceID string
	SessionID   string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "termioctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("termioctl", flag.ContinueOnError)
	gateway := fs.String("gateway", defaultGatewayURL, "gateway base URL")
	token := fs.String("token", "", "bearer token (default: TERMIO_TOKEN, then dev-token)")
	device := fs.String("device", "", "device id (default: first online device)")
	workspaceID := fs.String("workspace", "", "workspace id (default: device default workspace)")
	sessionID := fs.String("session", "", "session id sent with every call")
	timeoutMS := fs.Int("timeout", defaultTimeoutMS, "default tool timeout in milliseconds")
	payload := fs.String("json", "", "send one JSON tool call and exit")
	list := fs.Bool("list", false, "list online devices and exit")
	tools := fs.Bool("tools", false, "list tools for the selected device and exit")
	interactive := fs.Bool("interactive", false, "force the REPL even if stdin is not a TTY")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *timeoutMS <= 0 {
		return errors.New("--timeout must be positive")
	}

	base, err := normalizeGateway(*gateway)
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(*token)
	if tok == "" {
		tok = strings.TrimSpace(os.Getenv("TERMIO_TOKEN"))
	}
	if tok == "" {
		tok = "dev-token"
	}
	c := &client{
		baseURL:     base,
		token:       tok,
		http:        &http.Client{Timeout: time.Duration(*timeoutMS)*time.Millisecond + 10*time.Second},
		workspaceID: strings.TrimSpace(*workspaceID),
		sessionID:   strings.TrimSpace(*sessionID),
	}

	if *list {
		return c.printDevices()
	}
	dev, err := c.resolveDevice(*device)
	if err != nil {
		return err
	}
	if *device == "" {
		fmt.Fprintf(os.Stderr, "Using device %s (override with --device or :device)\n", dev)
	}
	if *tools {
		body, err := c.getJSON("/v1/devices/" + url.PathEscape(dev) + "/tools")
		if err != nil {
			return err
		}
		printPretty(body)
		return nil
	}
	if *payload != "" {
		return c.sendOnce(dev, *payload, *timeoutMS)
	}
	if !*interactive && !stdinIsTerminal() {
		return c.runPiped(dev, *timeoutMS)
	}
	return c.repl(dev, *timeoutMS)
}

func (c *client) resolveDevice(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	devices, err := c.devices()
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "", errors.New("no online devices; start `termio run` first")
	}
	return devices[0].ID, nil
}

func (c *client) devices() ([]deviceInfo, error) {
	body, err := c.getJSON("/v1/devices")
	if err != nil {
		return nil, err
	}
	var resp devicesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode devices: %w", err)
	}
	return resp.Devices, nil
}

func (c *client) printDevices() error {
	devices, err := c.devices()
	if err != nil {
		return err
	}
	fmt.Printf("%-24s %-20s %-14s %-10s %s\n", "ID", "NAME", "OS/ARCH", "VERSION", "DEFAULT WS")
	for _, d := range devices {
		fmt.Printf("%-24s %-20s %-14s %-10s %s\n", d.ID, d.Name, d.OS+"/"+d.Arch, d.Version, d.DefaultWorkspaceID)
	}
	return nil
}

func (c *client) printWorkspaces(device string) error {
	body, err := c.getJSON("/v1/devices/" + url.PathEscape(device) + "/workspaces")
	if err != nil {
		return err
	}
	printPretty(body)
	return nil
}

func (c *client) getJSON(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (c *client) send(device, raw string, defaultTimeoutMS int) ([]byte, error) {
	call, err := parseCall(raw, defaultTimeoutMS)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name":       call.Name,
		"arguments":  call.Arguments,
		"timeout_ms": call.TimeoutMS,
	}
	if call.CallID != "" {
		body["call_id"] = call.CallID
	}
	workspaceID := call.WorkspaceID
	if workspaceID == "" {
		workspaceID = c.workspaceID
	}
	if workspaceID != "" {
		body["workspace_id"] = workspaceID
	}
	sessionID := call.SessionID
	if sessionID == "" {
		sessionID = c.sessionID
	}
	if sessionID != "" {
		body["session_id"] = sessionID
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/v1/devices/" + url.PathEscape(device) + "/tools/call"
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c *client) sendOnce(device, raw string, defaultTimeoutMS int) error {
	out, err := c.send(device, raw, defaultTimeoutMS)
	if len(out) > 0 {
		printPretty(out)
	}
	if err != nil {
		return err
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(out, &result) == nil && result.IsError {
		return errors.New("tool returned isError=true")
	}
	return nil
}

func (c *client) runPiped(device string, defaultTimeoutMS int) error {
	scanner := newScanner(os.Stdin)
	hadError := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		out, err := c.send(device, line, defaultTimeoutMS)
		if len(out) > 0 {
			printPretty(out)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			hadError = true
		} else {
			var result struct {
				IsError bool `json:"isError"`
			}
			if json.Unmarshal(out, &result) == nil && result.IsError {
				hadError = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if hadError {
		return errors.New("one or more calls failed")
	}
	return nil
}

func (c *client) repl(device string, defaultTimeoutMS int) error {
	fmt.Println("Termio self-test REPL")
	fmt.Printf("Gateway:   %s\n", c.baseURL)
	fmt.Printf("Device:    %s\n", device)
	fmt.Printf("Workspace: %s\n", displayOr(c.workspaceID, "(device default)"))
	fmt.Printf("Session:   %s\n", displayOr(c.sessionID, "(none)"))
	fmt.Println("Type a JSON tool call, or :help for commands. Ctrl+C / :quit to exit.")
	fmt.Println()

	scanner := newScanner(os.Stdin)
	for {
		fmt.Printf("termio[%s %s %s]> ", device, displayOr(c.workspaceID, "default"), displayOr(c.sessionID, "-"))
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			quit, err := c.command(line, &device, &defaultTimeoutMS)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			if quit {
				return nil
			}
			continue
		}
		out, err := c.send(device, line, defaultTimeoutMS)
		if len(out) > 0 {
			printPretty(out)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
	}
}

func (c *client) command(line string, device *string, defaultTimeoutMS *int) (bool, error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case ":quit", ":q", ":exit":
		return true, nil
	case ":help", ":h", "?":
		printHelp()
		return false, nil
	case ":devices":
		return false, c.printDevices()
	case ":device":
		if len(fields) < 2 {
			fmt.Printf("current device: %s\n", *device)
			return false, nil
		}
		*device = fields[1]
		fmt.Printf("device: %s\n", *device)
		return false, nil
	case ":tools":
		body, err := c.getJSON("/v1/devices/" + url.PathEscape(*device) + "/tools")
		if err != nil {
			return false, err
		}
		printPretty(body)
		return false, nil
	case ":workspaces":
		return false, c.printWorkspaces(*device)
	case ":workspace", ":ws":
		if len(fields) < 2 {
			fmt.Printf("current workspace: %s\n", displayOr(c.workspaceID, "(device default)"))
			return false, nil
		}
		id := fields[1]
		if id == "-" || id == "default" || id == "reset" {
			c.workspaceID = ""
		} else {
			c.workspaceID = id
		}
		fmt.Printf("workspace: %s\n", displayOr(c.workspaceID, "(device default)"))
		return false, nil
	case ":session":
		if len(fields) < 2 {
			fmt.Printf("current session: %s\n", displayOr(c.sessionID, "(none)"))
			return false, nil
		}
		if fields[1] == "-" || fields[1] == "none" || fields[1] == "reset" {
			c.sessionID = ""
		} else {
			c.sessionID = fields[1]
		}
		fmt.Printf("session: %s\n", displayOr(c.sessionID, "(none)"))
		return false, nil
	case ":health":
		body, err := c.getJSON("/healthz")
		if err != nil {
			return false, err
		}
		printPretty(body)
		return false, nil
	case ":timeout":
		if len(fields) < 2 {
			fmt.Printf("current timeout: %dms\n", *defaultTimeoutMS)
			return false, nil
		}
		var ms int
		if _, err := fmt.Sscanf(fields[1], "%d", &ms); err != nil || ms <= 0 {
			return false, errors.New("usage: :timeout <milliseconds>")
		}
		*defaultTimeoutMS = ms
		c.http.Timeout = time.Duration(ms)*time.Millisecond + 10*time.Second
		fmt.Printf("timeout: %dms\n", ms)
		return false, nil
	default:
		return false, fmt.Errorf("unknown command %q; try :help", fields[0])
	}
}

func printHelp() {
	fmt.Print(`Commands:
  :help                 show this help
  :devices              list online devices
  :device <id>          switch device
  :tools                list tools for the current device
  :workspaces           list workspaces for the current device
  :workspace [id|-]     show or switch workspace (- resets to default)
  :session [id|-]       show, set, or clear the current session id
  :health               GET /healthz
  :timeout <ms>         set the default tool timeout
  :quit                 exit

JSON examples:
  {"name":"system_info","arguments":{}}
  {"name":"search_files","arguments":{"pattern":"*.go","max_results":5}}
  {"name":"read_file","arguments":{"path":"go.mod"}}
  {"name":"read_file","arguments":{"path":"../outside"}}
  {"name":"system_info","arguments":{},"workspace_id":"ws_docs","session_id":"chat-42"}
  {"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"system_info","arguments":{},"_meta":{"termio/workspaceId":"ws_docs","termio/sessionId":"chat-42"}}}

Reuse "call_id" to test idempotent replay (the same id in two workspaces is independent):
  {"name":"system_info","arguments":{},"call_id":"my-test-1"}
`)
}

func parseCall(raw string, defaultTimeoutMS int) (normalizedCall, error) {
	var w wireCall
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return normalizedCall{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if w.Method != "" {
		if w.Method != methodToolsCall {
			return normalizedCall{}, fmt.Errorf("unsupported method %q; use %q", w.Method, methodToolsCall)
		}
		if w.Params == nil {
			return normalizedCall{}, errors.New("tools/call envelope is missing params")
		}
		w = *w.Params
	}
	if strings.TrimSpace(w.Name) == "" {
		return normalizedCall{}, errors.New(`missing "name" (for example {"name":"system_info","arguments":{}})`)
	}
	args := w.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var object map[string]any
	if err := json.Unmarshal(args, &object); err != nil {
		return normalizedCall{}, fmt.Errorf(`"arguments" must be a JSON object: %w`, err)
	}
	timeout := w.TimeoutMS
	callID := w.CallID
	workspaceID := w.WorkspaceID
	sessionID := w.SessionID
	if w.Meta != nil {
		if w.Meta.TimeoutMS > 0 {
			timeout = w.Meta.TimeoutMS
		}
		if w.Meta.CallID != "" {
			callID = w.Meta.CallID
		}
		if w.Meta.WorkspaceID != "" {
			workspaceID = w.Meta.WorkspaceID
		}
		if w.Meta.SessionID != "" {
			sessionID = w.Meta.SessionID
		}
	}
	if timeout <= 0 {
		timeout = defaultTimeoutMS
	}
	return normalizedCall{
		Name:        w.Name,
		Arguments:   args,
		TimeoutMS:   timeout,
		CallID:      callID,
		WorkspaceID: strings.TrimSpace(workspaceID),
		SessionID:   strings.TrimSpace(sessionID),
	}, nil
}

func normalizeGateway(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("gateway URL is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported gateway scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("gateway URL must include a host")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/ws")
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func displayOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func printPretty(data []byte) {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		fmt.Println(strings.TrimSpace(string(data)))
		return
	}
	fmt.Println(out.String())
}

func newScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return scanner
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
