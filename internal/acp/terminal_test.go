package acp

import (
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"testing"
)

func TestTerminalWireShapes(t *testing.T) {
	for _, tt := range []struct {
		u    Update
		want string
	}{
		{TerminalUpdate{TerminalID: "t", Command: "printf <&>", Cwd: "/w"}, `{"sessionId":"s","update":{"sessionUpdate":"terminal_update","terminalId":"t","command":"printf <&>","cwd":"/w"}}`},
		{TerminalUpdate{TerminalID: "t", Output: &TerminalOutput{Data: "4g=="}, ExitStatus: &TerminalExitStatus{ExitCode: new(uint32(0))}}, `{"sessionId":"s","update":{"sessionUpdate":"terminal_update","terminalId":"t","output":{"data":"4g=="},"exitStatus":{"exitCode":0}}}`},
		{TerminalUpdate{TerminalID: "t", ExitStatus: &TerminalExitStatus{}}, `{"sessionId":"s","update":{"sessionUpdate":"terminal_update","terminalId":"t","exitStatus":{}}}`},
		{TerminalOutputChunk{TerminalID: "t", Data: "AP8="}, `{"sessionId":"s","update":{"sessionUpdate":"terminal_output_chunk","terminalId":"t","data":"AP8="}}`},
		{ToolCallUpdate{ToolCallID: "c", Content: []ToolCallContent{ToolTerminal("t")}}, `{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"c","content":[{"type":"terminal","terminalId":"t"}]}}`},
	} {
		b, err := jsonrpc.Marshal(SessionNotification{SessionID: "s", Update: tt.u})
		if err != nil || string(b) != tt.want {
			t.Fatalf("got %s, %v; want %s", b, err, tt.want)
		}
	}
}
