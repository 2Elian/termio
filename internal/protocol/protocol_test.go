package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	msg, err := NewRequest("g_1", MethodToolsCall, ToolCallParams{
		Name:      "search_files",
		Arguments: json.RawMessage(`{"pattern":"*.go"}`),
		Meta:      CallMeta{CallID: "call_1", TimeoutMS: 1500},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "g_1" || got.Method != MethodToolsCall {
		t.Fatalf("unexpected message: %+v", got)
	}
	var params ToolCallParams
	if err := DecodeParams(&got, &params); err != nil {
		t.Fatal(err)
	}
	if params.Name != "search_files" || params.Meta.CallID != "call_1" || params.Meta.TimeoutMS != 1500 {
		t.Fatalf("unexpected params: %+v", params)
	}
	if string(params.Arguments) != `{"pattern":"*.go"}` {
		t.Fatalf("arguments not preserved: %s", params.Arguments)
	}
}

func TestNewResultNil(t *testing.T) {
	msg, err := NewResult("g_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Result) != "{}" {
		t.Fatalf("result = %s, want {}", msg.Result)
	}
}

func TestDecodeParamsEmpty(t *testing.T) {
	var v struct {
		Name string `json:"name"`
	}
	if err := DecodeParams(&Message{}, &v); err != nil {
		t.Fatal(err)
	}
	if v.Name != "" {
		t.Fatalf("name = %q", v.Name)
	}
}

func TestDecodeResult(t *testing.T) {
	msg, err := NewResult("g_1", CallToolResult{
		Content: []ContentBlock{{Type: "text", Text: "ok"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result CallToolResult
	if err := DecodeResult(msg, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "ok" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestNewID(t *testing.T) {
	id := NewID("call_")
	if !strings.HasPrefix(id, "call_") || len(id) != len("call_")+16 {
		t.Fatalf("id = %q", id)
	}
	if id == NewID("call_") {
		t.Fatal("ids should be unique")
	}
}

func TestErrorString(t *testing.T) {
	var e *Error
	if e.Error() == "" {
		t.Fatal("nil error should still produce a string")
	}
	e = &Error{Code: CodeInvalidParams, Message: "bad"}
	if !strings.Contains(e.Error(), "bad") {
		t.Fatalf("error string = %q", e.Error())
	}
}
