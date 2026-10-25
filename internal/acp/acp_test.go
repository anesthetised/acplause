package acp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestSessionNotificationPutsDiscriminatorFirst(t *testing.T) {
	tests := []struct {
		update Update
		want   string
	}{
		{
			CompactionUpdate{CompactionID: "c1", Status: CompactionCompleted, Summary: []ContentBlock{Text("a < b")}},
			`{"sessionId":"s1","update":{"sessionUpdate":"compaction_update","compactionId":"c1","status":"completed","summary":[{"type":"text","text":"a < b"}]}}`,
		},
		{
			StateUpdate{State: StateRunning},
			`{"sessionId":"s1","update":{"sessionUpdate":"state_update","state":"running"}}`,
		},
		{
			AgentMessageChunk{MessageID: "m1", Content: Text("a < b")},
			`{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"a < b"}}}`,
		},
		{
			UsageUpdate{Used: 10, Size: 100, Cost: &Cost{Amount: 0.5, Currency: "USD"}},
			`{"sessionId":"s1","update":{"sessionUpdate":"usage_update","used":10,"size":100,"cost":{"amount":0.5,"currency":"USD"}}}`,
		},
		{
			ToolCallUpdate{ToolCallID: "t1", Content: []ToolCallContent{
				ToolDiff([]DiffChange{{Operation: DiffModify, Path: "/w/a.go", FileType: FileText}}, &DiffPatch{Format: PatchGit, Text: "p"}),
				ToolText("ok"),
			}},
			`{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","content":[` +
				`{"type":"diff","changes":[{"operation":"modify","path":"/w/a.go","fileType":"text"}],"patch":{"format":"git_patch","text":"p"}},` +
				`{"type":"content","content":{"type":"text","text":"ok"}}]}}`,
		},
	}
	for _, tt := range tests {
		got, err := jsonrpc.Marshal(SessionNotification{SessionID: "s1", Update: tt.update})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("got  %s\nwant %s", got, tt.want)
		}
	}
}

func TestContentBlockRoundTripsUnknownBlocks(t *testing.T) {
	in := `[{"type":"text","text":"hi","annotations":{"priority":1}},{"type":"_custom","payload":[1,2],"resource":"not an object","data":{},"mimeType":[]},{"type":"resource_link","uri":"file:///a.go","name":"a.go"},{"type":"image","data":"YWJj","mimeType":"image/png","uri":"file:///a.png","_future":true}]`
	var blocks []ContentBlock
	if err := json.Unmarshal([]byte(in), &blocks); err != nil {
		t.Fatal(err)
	}
	if blocks[0].Text != "hi" || blocks[1].Type != "_custom" || blocks[2].URI != "file:///a.go" {
		t.Fatalf("unexpected parse: %+v", blocks)
	}
	if blocks[3].Data != "YWJj" || blocks[3].MimeType != "image/png" {
		t.Fatal("image fields were not parsed")
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("round trip changed blocks:\n got %s\nwant %s", out, in)
	}
}

func TestContentImageEncoding(t *testing.T) {
	b, err := jsonrpc.Marshal(ContentBlock{Type: ContentImage, Data: "YWJj", MimeType: "image/png"})
	if err != nil || string(b) != `{"type":"image","data":"YWJj","mimeType":"image/png"}` {
		t.Fatalf("image encoding = %s, %v", b, err)
	}
	var block ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"image","data":{},"mimeType":"image/png"}`), &block); err == nil {
		t.Fatal("malformed image fields accepted")
	}
}

func TestValidation(t *testing.T) {
	cwd, _ := json.Marshal(t.TempDir())
	tests := []struct {
		name    string
		params  string
		into    any
		wantErr string
	}{
		{"initialize ok", `{"protocolVersion":2,"info":{"name":"c","version":"1"}}`, &InitializeRequest{}, ""},
		{"initialize no version", `{"info":{"name":"c","version":"1"}}`, &InitializeRequest{}, "protocolVersion is required"},
		{"initialize fractional", `{"protocolVersion":2.5,"info":{"name":"c","version":"1"}}`, &InitializeRequest{}, "cannot unmarshal"},
		{"initialize no info", `{"protocolVersion":2}`, &InitializeRequest{}, "info is required"},
		{"new relative cwd", `{"cwd":"src"}`, &NewSessionRequest{}, "cwd must be an absolute path"},
		{"new relative extra dir", `{"cwd":` + string(cwd) + `,"additionalDirectories":["x"]}`, &NewSessionRequest{}, "additionalDirectories must be an absolute path"},
		{"prompt missing", `{"sessionId":"s"}`, &PromptRequest{}, "prompt is required"},
		{"prompt untyped block", `{"sessionId":"s","prompt":[{"text":"x"}]}`, &PromptRequest{}, "content block type is required"},
		{"resume empty cursor", `{"sessionId":"s","cwd":` + string(cwd) + `,"replayFrom":{}}`, &ResumeSessionRequest{}, "replayFrom.type is required"},
		{"list null fields", `{"cwd":null,"cursor":null}`, &ListSessionsRequest{}, ""},
		{"config missing value", `{"sessionId":"s","configId":"model","type":"id"}`, &SetConfigOptionRequest{}, "value is required"},
		{"cancel no session", `{}`, &CancelSessionNotification{}, "sessionId is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := jsonrpc.DecodeParams(json.RawMessage(tt.params), tt.into)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
			if code := jsonrpc.AsError(err).Code; code != jsonrpc.CodeInvalidParams {
				t.Fatalf("got code %d, want %d", code, jsonrpc.CodeInvalidParams)
			}
		})
	}
}

func TestToolCallUpdateMerge(t *testing.T) {
	start := ToolCallUpdate{ToolCallID: "c1", Name: "bash", Title: "Run ls", Kind: ToolExecute, Status: ToolInProgress, RawInput: json.RawMessage(`{"command":"ls"}`)}
	end := ToolCallUpdate{ToolCallID: "c1", Status: ToolCompleted, Content: []ToolCallContent{ToolText("a\nb")}}
	got := start.Merge(end)
	if got.Status != ToolCompleted || got.Title != "Run ls" || string(got.RawInput) != `{"command":"ls"}` || len(got.Content) != 1 {
		t.Fatalf("unexpected merge: %+v", got)
	}
}
