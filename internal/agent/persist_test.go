package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func TestRestoreRepairsInterruptedTools(t *testing.T) {
	for _, status := range []acp.ToolCallStatus{acp.ToolPending, acp.ToolInProgress} {
		t.Run(string(status), func(t *testing.T) {
			assistant, err := json.Marshal(llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "done"}, {ID: "unfinished"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			answered, err := json.Marshal(toolResult("done", "already finished"))
			if err != nil {
				t.Fatal(err)
			}
			call, err := encodeEntry(acp.ToolCallUpdate{ToolCallID: "unfinished", Name: "bash", Status: status})
			if err != nil {
				t.Fatal(err)
			}
			s, err := restoreSession(context.Background(), deps{}, newCatalog(nil), &store.Loaded{
				Session: store.Session{ID: "restored", Cwd: t.TempDir(), State: "running"},
				History: [][]byte{assistant, answered}, Transcript: [][]byte{call},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.state.State != acp.StateIdle || len(s.history) != 4 {
				t.Fatalf("recovered state=%v, history=%+v", s.state, s.history)
			}
			if got := s.history[3]; got.Role != llm.RoleTool || got.ToolCallID != "unfinished" || got.Content != interrupted {
				t.Fatalf("recovered result: %+v", got)
			}
			got := s.transcript[0].(acp.ToolCallUpdate)
			if got.Status != acp.ToolFailed || got.Name != "bash" || string(got.RawOutput) != string(jsonString(interrupted)) {
				t.Fatalf("recovered replay: %+v", got)
			}
			_, history, transcript, _, err := s.pending()
			if err != nil || len(history) != 1 || len(transcript) != 1 {
				t.Fatalf("repair not queued for persistence: %v, %v, %v", history, transcript, err)
			}
			s.repair()
			if len(s.history) != 4 {
				t.Fatal("repair duplicated a tool result")
			}
		})
	}
}
