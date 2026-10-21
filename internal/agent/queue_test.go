package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

// lastID is the id of the request sent last.
func (c *acpClient) lastID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextID
}

func TestCancelRequestWithdrawsQueuedPrompt(t *testing.T) {
	busy := make(chan struct{}, 1)
	model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		busy <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := startAgent(t, t.TempDir(), model, nil)
	c.initialize()
	sid := c.newSession(t.TempDir())
	c.selectModel(sid, "m")
	first := c.prompt(sid, "one")
	<-busy
	second := c.prompt(sid, "two")
	c.notify("$/cancel_request", map[string]any{"requestId": c.lastID()})
	if r := c.await(second); r.Error == nil || r.Error.Code != -32800 {
		t.Fatalf("queued prompt: %+v", r)
	}
	c.notify(acp.MethodSessionCancel, map[string]any{"sessionId": sid})
	if r := c.await(first); r.Error != nil {
		t.Fatalf("running prompt: %+v", r.Error)
	}
	if idle := lastIdle(c.untilIdle(sid)); idle.Update.StopReason != string(acp.StopCancelled) {
		t.Fatalf("turn: %+v", idle)
	}
}

// gatedAcquireStore holds Acquire until the test settles it.
type gatedAcquireStore struct {
	Store
	entered chan struct{}
	release chan error
}

func (s *gatedAcquireStore) Acquire(ctx context.Context, id string) (*store.Loaded, error) {
	s.entered <- struct{}{}
	if err := <-s.release; err != nil {
		return nil, err
	}
	return s.Store.Acquire(ctx, id)
}

func TestPromptWaitsForPendingResume(t *testing.T) {
	for _, outcome := range []string{"success", "failed setup", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			gate := &gatedAcquireStore{entered: make(chan struct{}, 1), release: make(chan error, 1)}
			model := fakeModel(func(context.Context, llm.Request, func(llm.Delta)) (*llm.Completion, error) {
				return &llm.Completion{Content: "done", FinishReason: llm.FinishStop}, nil
			})
			c := startAgent(t, t.TempDir(), model, nil, func(st Store) Store { gate.Store = st; return gate })
			c.initialize()
			cwd := t.TempDir()
			sid := c.newSession(cwd)
			c.selectModel(sid, "m")
			resume := c.send(acp.MethodSessionResume, map[string]any{"sessionId": sid, "cwd": cwd})
			<-gate.entered
			prompt := c.prompt(sid, "hi")
			// Settle the resume only once the prompt is queued behind it.
			for queued := false; !queued; time.Sleep(time.Millisecond) {
				c.agent.mu.Lock()
				op := c.agent.operations[sid]
				queued = op != nil && op.previous != nil // set under mu, unlike op.prompt
				c.agent.mu.Unlock()
			}
			switch outcome {
			case "success":
				gate.release <- nil
				if r := c.await(resume); r.Error != nil {
					t.Fatalf("resume: %+v", r.Error)
				}
				if r := c.await(prompt); r.Error != nil {
					t.Fatalf("prompt: %+v", r.Error)
				}
				c.waitFor(sid, func(n notification) bool {
					return n.Update.Kind == "state_update" && n.Update.StopReason == string(acp.StopEndTurn)
				})
			case "failed setup":
				gate.release <- errors.New("disk on fire")
				if r := c.await(resume); r.Error == nil || r.Error.Code != -32603 {
					t.Fatalf("resume: %+v", r)
				}
				if r := c.await(prompt); r.Error == nil || r.Error.Code != -32603 {
					t.Fatalf("prompt after failed setup: %+v", r)
				}
			case "cancelled":
				c.notify("$/cancel_request", map[string]any{"requestId": c.lastID()})
				if r := c.await(prompt); r.Error == nil || r.Error.Code != -32800 {
					t.Fatalf("cancelled prompt: %+v", r)
				}
				gate.release <- nil
				if r := c.await(resume); r.Error != nil {
					t.Fatalf("resume: %+v", r.Error)
				}
			}
		})
	}
}
