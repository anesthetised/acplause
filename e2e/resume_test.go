package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResumeChangesRootsDuringModelRequest(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	release := make(chan struct{})
	oldDir, newDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{oldDir, newDir} {
		if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("shared"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	llm.enqueue(func(s *stream) {
		s.text("Waiting.")
		select {
		case <-release:
		case <-s.r.Context().Done():
			return
		}
		for i, path := range []string{"local.txt", filepath.Join(newDir, "shared.txt"), filepath.Join(oldDir, "shared.txt")} {
			args, _ := json.Marshal(map[string]string{"path": path})
			s.toolCall(i, path, "read_file", string(args))
		}
		s.finish("tool_calls")
	}, reply("Done."))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "local.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid := a.newSession(cwd, oldDir).SessionID
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "test/model"}, nil)
	a.accepted(a.prompt(sid, "read files"))
	a.updateWhere(sid, func(u update) bool { return u.is("agent_message_chunk") })
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{newDir}}, nil)
	close(release)
	a.untilIdle(sid)
	msgs := llm.request(1).Messages
	for i, want := range []string{"local", "shared", "outside the workspace"} {
		got, _ := msgs[3+i]["content"].(string)
		if (i < 2 && got != want) || (i == 2 && !strings.Contains(got, want)) {
			t.Errorf("tool result %d = %q, want %q", i, got, want)
		}
	}
	system := msgs[0]["content"].(string)
	if !strings.Contains(system, newDir) || strings.Contains(system, oldDir) {
		t.Fatalf("system prompt did not replace roots: %s", system)
	}
}
