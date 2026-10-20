package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const editBatchArgs = `{"path":"a.txt","edits":[{"old_text":"one","new_text":"ONE"},{"old_text":"ONE","new_text":"first"},{"old_text":"two\n","new_text":""},{"old_text":"caf\u00e9","new_text":"th\u00e9"}]}`

func TestEditBatchPermissionStalenessAndReplay(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"allow", "changed", "deleted", "directory", "reject", "cancel", "invalid", "plan", "outside"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t)
			args := editBatchArgs
			if kind == "invalid" {
				args = `{"path":"a.txt","edits":[{"old_text":"one","new_text":"ONE"},{"old_text":"missing","new_text":"new"}]}`
			} else if kind == "outside" {
				args = `{"path":"../outside","edits":[{"old_text":"one","new_text":"ONE"}]}`
			}
			llm.enqueue(callTool("edit_batch", args), reply("Done."))
			a, sid, cwd := started(t, llm, askingUser, withDataDir(data))
			if kind == "plan" {
				setMode(t, a, sid, "plan")
			}
			path := filepath.Join(cwd, "a.txt")
			original := "one\ntwo\ncaf\u00e9\n"
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			a.accepted(a.prompt(sid, "edit together"))
			var approved []diffBlock
			pending := 0
			if kind != "invalid" && kind != "plan" && kind != "outside" {
				pending = 1
				req := a.permissionRequest()
				p := decodePermission(t, req)
				if p.Subject.Type != "tool_call" || p.Subject.ToolCall == nil || p.Title != "Batch edit a.txt?" {
					t.Fatalf("batch permission: %s", req.Params)
				}
				approved = diffContent(t, update{Content: p.Subject.ToolCall.Content})
				if len(approved) != 1 || len(approved[0].Changes) != 1 || approved[0].Changes[0].Path != path || approved[0].Patch == nil || !strings.Contains(approved[0].Patch.Text, "+first\n+th\u00e9\n") || strings.Contains(approved[0].Patch.Text, "+ONE") {
					t.Fatalf("batch preview: %+v", approved)
				}
				if before, err := os.ReadFile(path); err != nil || string(before) != original {
					t.Fatalf("wrote before approval: %q, %v", before, err)
				}
				switch kind {
				case "changed":
					if err := os.WriteFile(path, []byte("one external change"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "deleted", "directory":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if kind == "directory" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					}
				case "cancel":
					a.notify("session/cancel", map[string]any{"sessionId": sid})
				}
				if kind != "cancel" {
					option := "allow_once"
					if kind == "reject" {
						option = "reject_once"
					}
					a.answer(req, selected(optionOfKind(t, req, option)))
				}
			}
			updates := a.untilIdle(sid)
			status, wantText := "failed", ""
			if kind == "allow" {
				status, wantText = "completed", "Batch edited"
			} else if kind == "cancel" {
				status, wantText = "cancelled", "Cancelled"
				if idle := updates[len(updates)-1]; idle.StopReason != "cancelled" {
					t.Fatalf("cancelled idle: %+v", idle)
				}
				// Continue so the cancelled call's result reaches the model.
				a.accepted(a.prompt(sid, "continue"))
				a.untilIdle(sid)
			} else {
				wantText = map[string]string{"changed": "re-read", "deleted": "re-read", "directory": "re-read", "reject": "rejected", "invalid": "edit 2", "plan": "Plan mode", "outside": "outside the workspace"}[kind]
			}
			var final update
			pendingCount := 0
			for _, u := range filter(updates, "tool_call_update") {
				if u.Status == "pending" {
					pendingCount++
				}
				if u.Status == status {
					final = u
				}
			}
			if final.ToolCallID == "" || pendingCount != pending {
				t.Fatalf("batch lifecycle: %v", kinds(updates))
			}
			if kind == "allow" && !reflect.DeepEqual(diffContent(t, final), approved) {
				t.Fatal("written diff differs from approval")
			}
			if kind != "allow" && kind != "cancel" && len(diffContent(t, final)) != 0 {
				t.Fatal("failed batch reported an applied diff")
			}
			messages := llm.request(1).Messages
			found := false
			for _, m := range messages {
				if m["role"] == "tool" && m["tool_call_id"] == "call_edit_batch" && strings.Contains(m["content"].(string), wantText) {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing model result %q: %+v", wantText, messages)
			}
			if kind == "deleted" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("deleted file recreated: %v", err)
				}
			} else if kind == "directory" {
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatalf("directory changed: %v", err)
				}
			} else {
				want := original
				if kind == "allow" {
					want = "first\nth\u00e9\n"
				} else if kind == "changed" {
					want = "one external change"
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Fatalf("file = %q, %v; want %q", got, err, want)
				}
			}
			// Restart replay retains the outcome without executing the batch.
			a.stop()
			b := start(t, llm, askingUser, withDataDir(data))
			b.initialize()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := filter(b.untilIdle(sid), "tool_call_update")
			if len(replay) != 1 || replay[0].Status != status || replay[0].ToolCallID != final.ToolCallID || replay[0].Name != "edit_batch" || (kind == "allow" && !reflect.DeepEqual(diffContent(t, replay[0]), approved)) {
				t.Fatalf("restored batch result: %+v", replay)
			}
		})
	}
}
