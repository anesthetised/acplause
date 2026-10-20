package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

const agentBatchArgs = `{"path":"a.txt","edits":[{"old_text":"one","new_text":"ONE"},{"old_text":"ONE","new_text":"first"},{"old_text":"two","new_text":"second"}]}`

func TestEditBatchPermissionAndHistoryOverProtocol(t *testing.T) {
	for _, kind := range []string{"allow", "changed", "deleted", "reject", "cancel", "invalid", "plan"} {
		t.Run(kind, func(t *testing.T) {
			model := make(scriptedModel, 2)
			args := agentBatchArgs
			if kind == "invalid" {
				args = `{"path":"a.txt","edits":[{"old_text":"one","new_text":"ONE"},{"old_text":"missing","new_text":"new"}]}`
			}
			model.callTool("batch", "edit_batch", args)
			requests := make(chan llm.Request, 1)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				requests <- req
				return &llm.Completion{FinishReason: llm.FinishStop}, nil
			}
			c := startAgent(t, t.TempDir(), model, nil)
			permission, release := make(chan struct{}, 1), make(chan struct{})
			defer close(release)
			var count atomic.Int32
			c.answer = func(context.Context) (string, string) {
				count.Add(1)
				permission <- struct{}{}
				<-release
				if kind == "reject" {
					return acp.OutcomeSelected, optionReject
				}
				return acp.OutcomeSelected, optionAllow
			}
			c.initialize()
			cwd := t.TempDir()
			path := filepath.Join(cwd, "a.txt")
			if err := os.WriteFile(path, []byte("one two"), 0o600); err != nil {
				t.Fatal(err)
			}
			sid := c.newSession(cwd)
			c.selectModel(sid, "m")
			if kind == "plan" {
				c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": sid, "configId": "mode", "type": "id", "value": "plan"}, nil)
			}
			c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("edit together")}}, nil)
			wantCount := int32(0)
			if kind != "invalid" && kind != "plan" {
				wantCount = 1
				select {
				case <-permission:
				case <-time.After(5 * time.Second):
					t.Fatal("no batch permission request")
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != "one two" {
					t.Fatalf("batch wrote before approval: %q, %v", data, err)
				}
				switch kind {
				case "changed":
					if err := os.WriteFile(path, []byte("one external change"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "deleted":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
				}
				if kind != "cancel" {
					release <- struct{}{}
				}
			}
			updates := c.untilIdle(sid)
			wantStatus := string(acp.ToolFailed)
			if kind == "allow" {
				wantStatus = string(acp.ToolCompleted)
			} else if kind == "cancel" {
				wantStatus = string(acp.ToolCancelled)
				if idle := lastIdle(updates); idle.Update.StopReason != "cancelled" {
					t.Fatalf("cancelled idle: %+v", idle)
				}
				// The cancelled call's paired result must reach a later round.
				release <- struct{}{}
				c.ok(acp.MethodSessionPrompt, map[string]any{"sessionId": sid, "prompt": []acp.ContentBlock{acp.Text("continue")}}, nil)
				c.untilIdle(sid)
			}
			found := false
			for _, n := range updates {
				if n.Update.Kind == "tool_call_update" && n.Update.Status == wantStatus {
					found = true
				}
			}
			if !found || count.Load() != wantCount {
				t.Fatalf("batch status=%s found=%v permissions=%d, want=%d", wantStatus, found, count.Load(), wantCount)
			}
			req := <-requests
			assertToolPairs(t, req.Messages)
			var result string
			for _, m := range req.Messages {
				if m.Role == llm.RoleTool && m.ToolCallID == "batch" {
					result = m.Content
				}
			}
			wantResult := map[string]string{"allow": "Batch edited", "changed": "re-read", "deleted": "re-read", "reject": "rejected", "cancel": "Cancelled", "invalid": "edit 2", "plan": "Plan mode"}[kind]
			if !strings.Contains(result, wantResult) {
				t.Fatalf("unpaired or wrong model result: %s", result)
			}
			if kind == "deleted" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("deleted file recreated: %v", err)
				}
			} else {
				want := "one two"
				if kind == "allow" {
					want = "first second"
				} else if kind == "changed" {
					want = "one external change"
				}
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("batch file = %q, %v; want %q", data, err, want)
				}
			}
		})
	}
}
