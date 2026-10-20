package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstructionsScopedLookupRefreshAndRestart(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"plan", "build"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			data := t.TempDir()
			llm := newMockLLM(t)
			a, sid, cwd := started(t, llm, withDataDir(data), askingUser)
			setMode(t, a, sid, mode)
			root := filepath.Join(cwd, "AGENTS.md")
			source := filepath.Join(cwd, "src", "AGENTS.md")
			if err := os.Mkdir(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			for path, text := range map[string]string{root: "First root marker.", source: "Scoped marker: caf\u00e9."} {
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			llm.enqueue(func(s *stream) {
				if err := os.WriteFile(root, []byte("Second root marker."), 0o600); err != nil {
					t.Error(err)
				}
				s.toolCall(0, "scoped", "get_instructions", `{"path":"src"}`)
				s.finish("tool_calls")
			}, func(s *stream) {
				if err := os.Remove(root); err != nil {
					t.Error(err)
				}
				s.toolCall(0, "bad-target", "get_instructions", `{"path":"src/AGENTS.md"}`)
				s.finish("tool_calls")
			}, reply("Inspected."))
			a.accepted(a.prompt(sid, "inspect instructions"))
			updates := a.untilIdle(sid)
			var completed, failed, reads int
			for _, u := range filter(updates, "tool_call_update") {
				if u.Status == "in_progress" && u.Kind == "read" {
					reads++
				}
				if u.Status == "completed" {
					completed++
					if !strings.Contains(u.text(), source) || !strings.Contains(u.text(), "Scoped marker: caf\u00e9.") || !strings.Contains(u.text(), "precedence") {
						t.Fatalf("scoped result: %+v", u)
					}
				}
				if u.Status == "failed" {
					failed++
					if !strings.Contains(u.text(), "not a directory") {
						t.Fatalf("nested failure: %+v", u)
					}
				}
			}
			if completed != 1 || failed != 1 || reads != 2 || updates[len(updates)-1].StopReason != "end_turn" {
				t.Fatalf("lookup lifecycle: %v", kinds(updates))
			}
			for i, want := range []string{"First root marker.", "Second root marker.", ""} {
				system := llm.request(i).Messages[0]["content"].(string)
				if (want != "" && !strings.Contains(system, want)) || (i > 0 && strings.Contains(system, "First root marker.")) || (i == 2 && strings.Contains(system, "Second root marker.")) {
					t.Fatalf("round %d system: %s", i, system)
				}
				if mode == "plan" && !strings.Contains(system, "Plan mode") {
					t.Fatal("refresh dropped Plan mode")
				}
			}
			original := llm.request(1).Messages[3]["content"].(string)
			if !strings.Contains(original, root) || !strings.Contains(original, source) || strings.Index(original, root) >= strings.Index(original, source) {
				t.Fatalf("source ordering: %s", original)
			}
			a.stop()
			if err := os.WriteFile(root, []byte("Restart root marker."), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, []byte("Changed scoped marker."), 0o600); err != nil {
				t.Fatal(err)
			}
			b := start(t, llm, withDataDir(data), askingUser)
			b.initialize()
			b.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "replayFrom": map[string]any{"type": "start"}}, nil)
			replay := b.untilIdle(sid)
			found := false
			for _, u := range filter(replay, "tool_call_update") {
				if u.Status == "completed" && strings.Contains(u.text(), "Scoped marker: caf\u00e9.") {
					found = true
				}
			}
			if !found {
				t.Fatal("restart lost scoped result replay")
			}
			llm.enqueue(reply("Continued."))
			b.accepted(b.prompt(sid, "continue"))
			b.untilIdle(sid)
			req := llm.request(3)
			if !strings.Contains(req.Messages[0]["content"].(string), "Restart root marker.") || req.Messages[3]["content"] != original {
				t.Fatalf("restart request: %+v", req.Messages)
			}
		})
	}
}

func TestInstructionsCompactRefreshCountsNewRootText(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, compactionModels)
	a, sid, cwd := seedCommandHistory(t, llm)
	// Within the 32 KiB file cap, but larger than this model's request budget.
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte(strings.Repeat("Root marker. ", 2000)), 0o600); err != nil {
		t.Fatal(err)
	}
	llm.enqueue(reply("Older task summarized."))
	a.accepted(a.prompt(sid, "/compact"))
	updates := a.untilIdle(sid)
	compactions := filter(updates, "compaction_update")
	if idle := updates[len(updates)-1]; idle.StopReason != "error" || len(compactions) != 2 || compactions[1].Status != "failed" || !strings.Contains(compactions[1].CompactionError, "could not fit") {
		t.Fatalf("manual compaction ignored refreshed root size: %v", kinds(updates))
	}
	if len(llm.request(2).Tools) != 0 {
		t.Fatal("manual compaction dispatched a normal tool round")
	}
}
