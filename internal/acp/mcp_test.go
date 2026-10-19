package acp

import (
	"encoding/json"
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestMCPServerConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, servers string
		valid         bool
	}{
		{"stdio", `[{"type":"stdio","name":"s","command":"/bin/server","args":["a"],"env":[{"name":"TOKEN","value":"opaque"}]}]`, true},
		{"http", `[{"type":"http","name":"s","url":"http://127.0.0.1/mcp","headers":[{"name":"Authorization","value":"Bearer opaque"}]}]`, true},
		{"future", `[{"type":"_future","command":42,"env":false}]`, true},
		{"relative", `[{"type":"stdio","name":"s","command":"server"}]`, false},
		{"duplicate", `[{"type":"stdio","name":"s","command":"/bin/a"},{"type":"http","name":"s","url":"https://example.com"}]`, false},
		{"scheme", `[{"type":"http","name":"s","url":"file:///tmp/a"}]`, false},
		{"header injection", `[{"type":"http","name":"s","url":"https://example.com","headers":[{"name":"X-Key","value":"a\r\nb"}]}]`, false},
		{"reserved header", `[{"type":"http","name":"s","url":"https://example.com","headers":[{"name":"Mcp-Session-Id","value":"other"}]}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var req NewSessionRequest
			err := jsonrpc.DecodeParams(json.RawMessage(`{"cwd":"/tmp","mcpServers":`+tt.servers+`}`), &req)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v: %v", tt.valid, err)
			}
			if tt.name == "future" && err == nil {
				got, err := jsonrpc.Marshal(req.MCPServers)
				if err != nil || string(got) != tt.servers {
					t.Fatalf("future transport changed: %s, %v", got, err)
				}
			}
		})
	}
}
