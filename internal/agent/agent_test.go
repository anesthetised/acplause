package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// fakeModel answers chat requests with a function.
type fakeModel func(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error)

func (f fakeModel) Chat(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
	return f(ctx, req, onDelta)
}

// recorder collects session updates.
type recorder chan acp.Update

func (r recorder) Notify(_ string, params any) error {
	r <- params.(acp.SessionNotification).Update
	return nil
}

// Call allows every permission request.
func (r recorder) Call(_ context.Context, _ string, _, result any) error {
	resp := result.(*acp.RequestPermissionResponse)
	resp.Outcome.Outcome, resp.Outcome.OptionID = acp.OutcomeSelected, optionAllow
	return nil
}

// untilIdle returns the updates up to and including the next idle state.
func (r recorder) untilIdle(t *testing.T) []acp.Update {
	t.Helper()
	var updates []acp.Update
	for {
		select {
		case u := <-r:
			updates = append(updates, u)
			if s, ok := u.(acp.StateUpdate); ok && s.State == acp.StateIdle {
				return updates
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no idle state; got %v", updates)
		}
	}
}

func testSession(t *testing.T, model Model) (*Session, recorder) {
	t.Helper()
	out := make(recorder, 64)
	s, err := newSession(context.Background(), deps{out: out, model: model, maxRounds: 3}, newCatalog([]llm.Model{{ID: "m"}}), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err := s.SetConfigOption(configModel, acp.ConfigValueID, json.RawMessage(`"m"`)); err != nil {
		t.Fatal(err)
	}
	return s, out
}

func TestTurnRunsToolsAndKeepsHistoryValid(t *testing.T) {
	var requests []llm.Request
	model := fakeModel(func(_ context.Context, req llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
		requests = append(requests, req)
		if len(requests) == 1 {
			return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "c1", Function: llm.FunctionCall{Name: "list_dir", Arguments: `{}`}}}}, nil
		}
		onDelta(llm.Delta{Kind: llm.DeltaText, Text: "done"})
		return &llm.Completion{Content: "done", FinishReason: llm.FinishStop}, nil
	})
	s, out := testSession(t, model)

	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("look around")})
	var kinds []string
	for _, u := range out.untilIdle(t) {
		kinds = append(kinds, fmt.Sprintf("%T", u))
	}
	want := []string{"acp.UserMessage", "acp.SessionInfoUpdate", "acp.StateUpdate", "acp.ToolCallUpdate", "acp.ToolCallUpdate", "acp.AgentMessageChunk", "acp.StateUpdate"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("got %v, want %v", kinds, want)
	}

	roles := func(msgs []llm.Message) []string {
		var r []string
		for _, m := range msgs {
			r = append(r, m.Role)
		}
		return r
	}
	if got := roles(requests[1].Messages); !slices.Equal(got, []string{"system", "user", "assistant", "tool"}) {
		t.Fatalf("second request roles: %v", got)
	}
}

func TestCancelledTurnEndsIdleCancelled(t *testing.T) {
	model := fakeModel(func(ctx context.Context, _ llm.Request, onDelta func(llm.Delta)) (*llm.Completion, error) {
		onDelta(llm.Delta{Kind: llm.DeltaReasoning, Text: "hmm"})
		<-ctx.Done()
		return nil, ctx.Err()
	})
	s, out := testSession(t, model)
	s.Submit(context.Background(), []acp.ContentBlock{acp.Text("wait")})
	for u := range out {
		if _, ok := u.(acp.AgentThoughtChunk); ok {
			break
		}
	}
	s.Cancel()
	updates := out.untilIdle(t)
	if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopCancelled || idle.Error != nil {
		t.Fatalf("idle: %+v", idle)
	}
}

func TestResumeRetainsRootsUntilActiveToolFinishes(t *testing.T) {
	s, _ := testSession(t, nil)
	if err := os.WriteFile(filepath.Join(s.cwd, "a.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	active := s.acquireToolbox()
	call, err := active.Prepare("read_file", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	// Several resumes must retain only the roots still used by this call.
	for range 2 {
		if err := s.Resume([]string{t.TempDir()}, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := call.Run(context.Background()); got.Failed || got.Output != "content" {
		t.Fatalf("resume interrupted a prepared tool: %+v", got)
	}
	s.releaseToolbox(active)
	if got := call.Run(context.Background()); !got.Failed {
		t.Fatal("retired roots remain open after tool completion")
	}
	current := s.acquireToolbox()
	defer s.releaseToolbox(current)
	call, err = current.Prepare("read_file", `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := call.Run(context.Background()); got.Failed || got.Output != "content" {
		t.Fatalf("current roots were closed: %+v", got)
	}
}

func TestCatalog(t *testing.T) {
	c := newCatalog([]llm.Model{
		{ID: "b/reasons", ContextLength: 100, SupportedParameters: []string{"tools", "reasoning"}},
		{ID: "a/plain", SupportedParameters: []string{"tools"}},
		{ID: "c/no-tools", SupportedParameters: []string{"temperature"}},
		{ID: "d/unknown"},
		{ID: ""}, // Empty ids are reserved for the unselected placeholder.
	})
	var ids []string
	for _, v := range c.choices {
		ids = append(ids, v.Value)
	}
	if want := []string{"a/plain", "b/reasons", "d/unknown"}; !slices.Equal(ids, want) {
		t.Fatalf("choices %v, want %v", ids, want)
	}
	if len(c.options("b/reasons", effortDefault)) != 2 || len(c.options("a/plain", effortDefault)) != 1 || len(c.options("d/unknown", effortDefault)) != 1 {
		t.Fatal("reasoning effort must be offered only for models known to reason")
	}
	if c.contextLength("b/reasons") != 100 || c.contextLength("d/unknown") != 0 {
		t.Fatal("unexpected context lengths")
	}
}

func TestCursor(t *testing.T) {
	for _, n := range []int{0, 50, 12345} {
		if got, err := decodeCursor(encodeCursor(n)); err != nil || got != n {
			t.Fatalf("round trip %d: %d, %v", n, got, err)
		}
	}
	for _, bad := range []string{"!!", encodeCursor(-1), "eA"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

func TestPromptText(t *testing.T) {
	var blocks []acp.ContentBlock
	_ = json.Unmarshal([]byte(`[{"type":"text","text":"Fix the bug\nin parsing"},{"type":"resource_link","uri":"file:///w/a.go","name":"a.go"},{"type":"image","data":"…","mimeType":"image/png"}]`), &blocks)
	if got, want := promptText(blocks), "Fix the bug\nin parsing\n\n[a.go](file:///w/a.go)"; got != want {
		t.Fatalf("promptText: %q, want %q", got, want)
	}
	if got := title(blocks); got != "Fix the bug" {
		t.Fatalf("title: %q", got)
	}
	if got := promptText(nil); got != "(empty message)" {
		t.Fatalf("empty prompt: %q", got)
	}
}

func TestRawInput(t *testing.T) {
	if got := string(rawInput(`{"a":1}`)); got != `{"a":1}` {
		t.Fatalf("valid JSON: %s", got)
	}
	if got := string(rawInput(`{"a":`)); got != `"{\"a\":"` {
		t.Fatalf("invalid JSON: %s", got)
	}
}
