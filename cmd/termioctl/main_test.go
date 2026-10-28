package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeGateway(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://127.0.0.1:8787", "http://127.0.0.1:8787", false},
		{"http://127.0.0.1:8787/", "http://127.0.0.1:8787", false},
		{"ws://127.0.0.1:8787/ws", "http://127.0.0.1:8787", false},
		{"wss://gateway.example.com/ws", "https://gateway.example.com", false},
		{"gateway.example.com", "http://gateway.example.com", false},
		{"ftp://gateway.example.com", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeGateway(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("%q: expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("%q: got %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseCallDirect(t *testing.T) {
	call, err := parseCall(`{"name":"search_files","arguments":{"pattern":"*.go"},"timeout_ms":5000,"call_id":"c1","workspace_id":"ws_docs","session_id":"chat-1"}`, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if call.Name != "search_files" || call.TimeoutMS != 5000 || call.CallID != "c1" {
		t.Fatalf("call = %+v", call)
	}
	if call.WorkspaceID != "ws_docs" || call.SessionID != "chat-1" {
		t.Fatalf("scope = %+v", call)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["pattern"] != "*.go" {
		t.Fatalf("args = %v", args)
	}
}

func TestParseCallJSONRPC(t *testing.T) {
	raw := `{
		"jsonrpc":"2.0",
		"id":"1",
		"method":"tools/call",
		"params":{
			"name":"system_info",
			"arguments":{},
			"_meta":{
				"termio/callId":"c2",
				"termio/timeoutMs":7000,
				"termio/workspaceId":"ws_a",
				"termio/sessionId":"s1"
			}
		}
	}`
	call, err := parseCall(raw, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if call.Name != "system_info" || call.CallID != "c2" || call.TimeoutMS != 7000 {
		t.Fatalf("call = %+v", call)
	}
	if call.WorkspaceID != "ws_a" || call.SessionID != "s1" {
		t.Fatalf("scope = %+v", call)
	}
}

func TestParseCallErrors(t *testing.T) {
	cases := []string{
		`{}`,
		`{"name":"x","arguments":[]}`,
		`{"method":"tools/list","params":{}}`,
		`{"method":"tools/call"}`,
		`not json`,
	}
	for _, raw := range cases {
		if _, err := parseCall(raw, 30000); err == nil {
			t.Fatalf("%s: expected error", raw)
		}
	}
}

func TestSend(t *testing.T) {
	var gotAuth, gotPath, gotName string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if name, _ := gotBody["name"].(string); name != "" {
			gotName = name
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"isError":false}`)
	}))
	defer server.Close()

	c := &client{
		baseURL:     server.URL,
		token:       "tok",
		http:        server.Client(),
		workspaceID: "ws_default",
		sessionID:   "session-default",
	}
	out, err := c.send("dev_1", `{"name":"system_info","arguments":{}}`, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"ok"`) {
		t.Fatalf("out = %s", out)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotPath != "/v1/devices/dev_1/tools/call" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotName != "system_info" {
		t.Fatalf("name = %q, body = %v", gotName, gotBody)
	}
	if gotBody["timeout_ms"].(float64) != 30000 {
		t.Fatalf("timeout = %v", gotBody["timeout_ms"])
	}
	if gotBody["workspace_id"] != "ws_default" || gotBody["session_id"] != "session-default" {
		t.Fatalf("scope defaults not sent: %v", gotBody)
	}
}

func TestSendBodyScopeOverridesClientDefaults(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer server.Close()

	c := &client{
		baseURL:     server.URL,
		token:       "tok",
		http:        server.Client(),
		workspaceID: "ws_default",
		sessionID:   "session-default",
	}
	_, err := c.send("dev_1", `{"name":"system_info","arguments":{},"workspace_id":"ws_other","session_id":"chat-9"}`, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["workspace_id"] != "ws_other" || gotBody["session_id"] != "chat-9" {
		t.Fatalf("body scope = %v", gotBody)
	}
}

func TestSendHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":-32602,"message":"bad"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	c := &client{baseURL: server.URL, token: "tok", http: server.Client()}
	out, err := c.send("dev_1", `{"name":"nope","arguments":{}}`, 30000)
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if !strings.Contains(string(out), "bad") {
		t.Fatalf("out = %s", out)
	}
}
