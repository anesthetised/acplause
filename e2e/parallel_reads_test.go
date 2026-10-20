package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParallelReadBarriersMixedResultsCancelAndReplay(t *testing.T) {
	t.Parallel()
	for _, workers := range []string{"1", "4"} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("workers=%s/cancel=%v", workers, cancel), func(t *testing.T) {
				t.Parallel()
				llm := newMockLLM(t, compactionModels)
				names := []string{"read_file", "list_dir", "write_file", "read_file", "search_code", "get_instructions", "list_dir"}
				args := []string{`{"path":"marker"}`, `{}`, `{"path":"marker","content":"new marker"}`, `{"path":"marker"}`, `{"pattern":"[","regex":true}`, `{"path":"."}`, `{}`}
				llm.enqueue(func(s *stream) {
					for i, name := range names {
						s.toolCall(i, fmt.Sprint(i), name, args[i])
					}
					s.finish("tool_calls")
				}, reply("Done."))
				data := t.TempDir()
				a, sid, cwd := started(t, llm, askingUser, withDataDir(data), withEnv("ACPLAUSE_READ_CONCURRENCY="+workers))
				if err := os.WriteFile(filepath.Join(cwd, "marker"), []byte("old marker"), 0o600); err != nil {
					t.Fatal(err)
				}
				a.accepted(a.prompt(sid, "inspect and change"))
				permission := a.permissionRequest()
				a.updateWhere(sid, func(u update) bool { return u.SessionUpdate == "state_update" && u.State == "requires_action" })
				if got, err := os.ReadFile(filepath.Join(cwd, "marker")); err != nil || string(got) != "old marker" {
					t.Fatal("write crossed its permission barrier")
				}
				if cancel {
					a.notify("session/cancel", map[string]any{"sessionId": sid})
				} else {
					a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
				}
				updates := a.untilIdle(sid)
				idle := updates[len(updates)-1]
				if cancel {
					if idle.StopReason != "cancelled" {
						t.Fatal("cancel lost final idle")
					}
					a.answer(permission, selected(optionOfKind(t, permission, "allow_once")))
					a.accepted(a.prompt(sid, "continue"))
					a.untilIdle(sid)
				} else if idle.StopReason != "end_turn" {
					t.Fatalf("idle: %+v", idle)
				}
				req := llm.request(1)
				assertBoundedChat(t, req)
				var results []map[string]any
				for _, m := range req.Messages {
					if m["role"] == "tool" {
						results = append(results, m)
					}
				}
				if len(results) != len(names) {
					t.Fatal("missing sibling result")
				}
				for i, m := range results {
					if m["tool_call_id"] != fmt.Sprint(i) {
						t.Fatal("history reordered by completion")
					}
					text := m["content"].(string)
					if i == 0 && text != "old marker" {
						t.Fatal("first read crossed write barrier")
					}
					if cancel && i >= 2 && !strings.Contains(text, "Cancelled") {
						t.Fatal("cancel did not pair later calls")
					}
					if !cancel && i == 3 && text != "new marker" {
						t.Fatal("later read started before write")
					}
					if !cancel && i == 4 && !strings.Contains(text, "invalid regex") {
						t.Fatal("failed read lost its result")
					}
				}
				want := "new marker"
				if cancel {
					want = "old marker"
				}
				if got, err := os.ReadFile(filepath.Join(cwd, "marker")); err != nil || string(got) != want {
					t.Fatal("write violated permission/cancellation")
				}
				a.stop()
				b := start(t, llm, askingUser, withDataDir(data), withEnv("ACPLAUSE_READ_CONCURRENCY="+workers))
				b.initialize()
				b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
				replay := b.untilIdle(sid)
				cards := filter(replay, "tool_call_update")
				wantCards := 7
				if cancel {
					wantCards = 3
				}
				if len(cards) != wantCards || llm.requestCount() != 2 {
					t.Fatalf("restart lost outcomes or replayed work: %v", kinds(replay))
				}
				for _, card := range cards {
					if card.Status == "pending" || card.Status == "in_progress" {
						t.Fatal("restart retained unfinished result")
					}
				}
			})
		}
	}
}
