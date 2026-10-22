package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// runBash prompts one bash call, returning the turn's updates.
func runBash(t *testing.T, c *acpClient, model scriptedModel, sid, command string) []notification {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": command})
	model.callTool("b", "bash", string(args))
	model.reply("ok", llm.FinishStop, nil)
	c.await(c.prompt(sid, "run"))
	return c.untilIdle(sid)
}

func (c *acpClient) permissionKinds(i int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var p struct{ Options []struct{ Kind string } }
	_ = json.Unmarshal(c.permissions[i], &p)
	var kinds []string
	for _, o := range p.Options {
		kinds = append(kinds, o.Kind)
	}
	return kinds
}

func (c *acpClient) permissionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.permissions)
}

func anyStatus(updates []notification, status acp.ToolCallStatus) bool {
	return slices.ContainsFunc(updates, func(u notification) bool { return u.Update.Status == string(status) })
}

func TestRememberedShellAllow(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionAllowAlways }
	c.mu.Unlock()

	runBash(t, c, model, sid, "touch ran")
	if got := c.permissionKinds(0); !slices.Equal(got, []string{"allow_once", "allow_always", "reject_once", "reject_always"}) {
		t.Fatalf("options: %v", got)
	}
	// The same command runs without a request; a different spelling asks.
	os.Remove(filepath.Join(cwd, "ran"))
	updates := runBash(t, c, model, sid, "touch ran")
	if c.permissionCount() != 1 || !anyStatus(updates, acp.ToolCompleted) {
		t.Fatalf("remembered allow asked again or failed: requests=%d %+v", c.permissionCount(), updates)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); err != nil {
		t.Fatal("remembered command did not run")
	}
	runBash(t, c, model, sid, "touch  ran")
	if c.permissionCount() != 2 {
		t.Fatalf("near-matching command was not asked about: %d requests", c.permissionCount())
	}
}

func TestRememberedShellReject(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionRejectAlways }
	c.mu.Unlock()

	if updates := runBash(t, c, model, sid, "touch ran"); !anyStatus(updates, acp.ToolFailed) {
		t.Fatalf("tool not failed: %+v", updates)
	}
	// Even a client that would now allow is not asked: the denial is remembered.
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionAllow }
	c.mu.Unlock()
	if updates := runBash(t, c, model, sid, "touch ran"); !anyStatus(updates, acp.ToolFailed) || c.permissionCount() != 1 {
		t.Fatalf("remembered reject: requests=%d %+v", c.permissionCount(), updates)
	}
	if updates := runBash(t, c, model, sid, "touch ran"); !slices.ContainsFunc(updates, func(u notification) bool {
		return strings.HasSuffix(u.Update.Title, "(rejected earlier this session)")
	}) {
		t.Fatalf("remembered reject not marked: %+v", updates)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); err == nil {
		t.Fatal("command ran after reject_always")
	}
}

func TestShellDecisionsNotRecordedWithoutExplicitChoice(t *testing.T) {
	for _, tc := range []struct{ name, outcome, option string }{
		{"dismissed", "cancelled", ""},
		{"once", acp.OutcomeSelected, optionAllow},
		{"unknown option", acp.OutcomeSelected, "allow_forever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := make(scriptedModel, 8)
			c := startAgent(t, t.TempDir(), model, nil)
			c.initialize()
			sid := c.newSession(t.TempDir())
			c.selectModel(sid, "m")
			c.mu.Lock()
			c.answer = func(context.Context) (string, string) { return tc.outcome, tc.option }
			c.mu.Unlock()
			runBash(t, c, model, sid, "true")
			runBash(t, c, model, sid, "true")
			if c.permissionCount() != 2 {
				t.Fatalf("a decision was remembered: %d requests", c.permissionCount())
			}
		})
	}
}

func TestOtherToolsOfferOnceOptionsOnly(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	sid := c.newSession(t.TempDir())
	c.selectModel(sid, "m")
	model.callTool("w", "write_file", `{"path":"a.txt","content":"x"}`)
	model.reply("ok", llm.FinishStop, nil)
	c.await(c.prompt(sid, "write"))
	c.untilIdle(sid)
	if got := c.permissionKinds(0); !slices.Equal(got, []string{"allow_once", "reject_once"}) {
		t.Fatalf("write_file options: %v", got)
	}
}

func TestRememberedDecisionBoundAndRootsReset(t *testing.T) {
	s, _ := testSession(t, fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
		return &llm.Completion{}, nil
	}))
	for i := range maxRemembered {
		s.remember(strings.Repeat("x", i+1), true, 0)
	}
	if ok, _ := s.canRemember(); ok {
		t.Fatal("always options offered at the bound")
	}
	s.remember("one more", true, 0)
	if _, ok := s.remembered("one more"); ok {
		t.Fatal("decision stored beyond the bound")
	}
	s.remember("x", false, 0) // replacing at the bound is allowed
	if allow, ok := s.remembered("x"); !ok || allow {
		t.Fatal("replacement lost")
	}
	// A new session in another cwd never sees them.
	other, _ := testSession(t, fakeModel(nil))
	if _, ok := other.remembered("x"); ok {
		t.Fatal("decisions leaked across sessions")
	}
}

func TestRememberedShellDecisionClearedByRootsChange(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd, dirA, dirB := t.TempDir(), t.TempDir(), t.TempDir()
	var created acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd, "additionalDirectories": []string{dirA}}, &created)
	sid := created.SessionID
	c.selectModel(sid, "m")
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionAllowAlways }
	c.mu.Unlock()
	runBash(t, c, model, sid, "true")
	runBash(t, c, model, sid, "true")
	if c.permissionCount() != 1 {
		t.Fatalf("not remembered: %d requests", c.permissionCount())
	}
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{dirB}}, nil)
	c.untilIdle(sid)
	runBash(t, c, model, sid, "true")
	if c.permissionCount() != 2 {
		t.Fatalf("roots change kept the decision: %d requests", c.permissionCount())
	}
}

func TestRememberedShellDecisionDoesNotBypassPlanMode(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd := t.TempDir()
	sid := c.newSession(cwd)
	c.selectModel(sid, "m")
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionAllowAlways }
	c.mu.Unlock()
	runBash(t, c, model, sid, "touch ran")
	os.Remove(filepath.Join(cwd, "ran"))
	c.ok(acp.MethodSessionSetConfig, map[string]any{"sessionId": sid, "configId": configMode, "type": "id", "value": modePlan}, nil)
	// A stale call in Plan mode fails before memory is consulted.
	model.callTool("b", "bash", `{"command":"touch ran"}`)
	model.reply("ok", llm.FinishStop, nil)
	c.await(c.prompt(sid, "run"))
	if updates := c.untilIdle(sid); !anyStatus(updates, acp.ToolFailed) {
		t.Fatalf("tool not failed in Plan mode: %+v", updates)
	}
	if _, err := os.Stat(filepath.Join(cwd, "ran")); err == nil {
		t.Fatal("remembered allow bypassed Plan mode")
	}
}

// An "always" answer given after a roots change must not outlive the change
// it raced with (ADR 0029 section 8).
func TestRememberedAnswerAfterRootsChangeIsDropped(t *testing.T) {
	model := make(scriptedModel, 8)
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	cwd, dirA, dirB := t.TempDir(), t.TempDir(), t.TempDir()
	var created acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, map[string]any{"cwd": cwd, "additionalDirectories": []string{dirA}}, &created)
	sid := created.SessionID
	c.selectModel(sid, "m")

	asked, release := make(chan struct{}), make(chan struct{})
	c.mu.Lock()
	c.answer = func(context.Context) (string, string) {
		close(asked)
		<-release
		return acp.OutcomeSelected, optionAllowAlways
	}
	c.mu.Unlock()
	args, _ := json.Marshal(map[string]string{"command": "true"})
	model.callTool("b", "bash", string(args))
	model.reply("ok", llm.FinishStop, nil)
	c.await(c.prompt(sid, "run"))
	<-asked
	c.ok(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd, "additionalDirectories": []string{dirB}}, nil)
	close(release)
	c.untilIdle(sid)

	c.mu.Lock()
	c.answer = func(context.Context) (string, string) { return acp.OutcomeSelected, optionAllow }
	c.mu.Unlock()
	runBash(t, c, model, sid, "true")
	if c.permissionCount() != 2 {
		t.Fatalf("decision made under the old roots was kept: %d requests", c.permissionCount())
	}
}

func TestRememberedLogKeyIsKeyed(t *testing.T) {
	s, _ := testSession(t, fakeModel(nil))
	var buf bytes.Buffer
	s.deps.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const command = "git push --force"
	s.logRemembered(command, true)
	s.logRemembered(command, true)
	s.logRemembered("other", true)

	var keys []string
	for line := range strings.Lines(buf.String()) {
		var entry struct{ Key string }
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, entry.Key)
	}
	if len(keys) != 3 || keys[0] != keys[1] || keys[0] == keys[2] {
		t.Fatalf("keys should correlate one command and tell commands apart: %v", keys)
	}
	plain := sha256.Sum256([]byte(s.rememberKey(command)))
	if keys[0] == hex.EncodeToString(plain[:6]) {
		t.Fatal("key is an unkeyed hash a dictionary could reproduce")
	}
	if strings.Contains(buf.String(), command) {
		t.Fatal("command text logged")
	}
}
