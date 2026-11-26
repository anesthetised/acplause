package acp

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestMCPServerConfiguration(t *testing.T) {
	cwd := t.TempDir()
	command, _ := json.Marshal(filepath.Join(cwd, "server"))
	for _, tt := range []struct {
		name, servers string
		valid         bool
	}{
		{"stdio", `[{"type":"stdio","name":"s","command":` + string(command) + `,"args":["a"],"env":[{"name":"TOKEN","value":"opaque"}]}]`, true},
		{"http", `[{"type":"http","name":"s","url":"http://127.0.0.1/mcp","headers":[{"name":"Authorization","value":"Bearer opaque"}]}]`, true},
		{"custom", `[{"type":"_custom","command":42,"env":false}]`, false},
		{"future", `[{"type":"future","name":"s"}]`, false},
		{"legacy SSE", `[{"type":"sse","name":"s","url":"https://example.com"}]`, false},
		{"unknown field cannot override transport", `[{"type":"sse","Type":"http","name":"s","url":"https://example.com"}]`, false},
		{"relative", `[{"type":"stdio","name":"s","command":"server"}]`, false},
		{"duplicate", `[{"type":"stdio","name":"s","command":` + string(command) + `},{"type":"http","name":"s","url":"https://example.com"}]`, false},
		{"scheme", `[{"type":"http","name":"s","url":"file:///tmp/a"}]`, false},
		{"header injection", `[{"type":"http","name":"s","url":"https://example.com","headers":[{"name":"X-Key","value":"a\r\nb"}]}]`, false},
		{"transport-owned header", `[{"type":"http","name":"s","url":"https://example.com","headers":[{"name":"mCP-sESSiOn-iD","value":"other"}],"_extension":true}]`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var req NewSessionRequest
			var resume ResumeSessionRequest
			raw, _ := json.Marshal(map[string]any{"sessionId": "s", "cwd": cwd, "mcpServers": json.RawMessage(tt.servers)})
			err := jsonrpc.DecodeParams(raw, &req)
			for _, err := range []error{err, jsonrpc.DecodeParams(raw, &resume)} {
				if (err == nil) != tt.valid {
					t.Fatalf("valid=%v: %v", tt.valid, err)
				}
				if rpcErr, ok := errors.AsType[*jsonrpc.Error](err); err != nil && (!ok || rpcErr.Code != jsonrpc.CodeInvalidParams) {
					t.Fatalf("expected invalid params, got %v", err)
				}
			}
			if tt.valid {
				got, err := jsonrpc.Marshal(req.MCPServers)
				if err != nil || string(got) != tt.servers {
					t.Fatalf("MCP config changed: %s, %v", got, err)
				}
			}
		})
	}
}
