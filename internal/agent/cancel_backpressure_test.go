package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

type pausedOutput struct {
	release <-chan struct{}
	bytes.Buffer
}

func (out *pausedOutput) Write(b []byte) (int, error) {
	<-out.release
	return out.Buffer.Write(b)
}

func TestCancelDispatchesNextSessionWithFullOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Each worker stays inside Chat while the reader admits and cancels
		// queued submissions. Neither worker needs the session lock here.
		releaseModels := make(chan struct{})
		resumeModels := sync.OnceFunc(func() { close(releaseModels) })
		turns := make(chan context.Context, 2)
		model := fakeModel(func(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
			turns <- ctx
			<-releaseModels
			return &llm.Completion{FinishReason: llm.FinishStop}, nil
		})
		first, _ := testSession(t, model)
		other, _ := testSession(t, model)
		first.Submit(t.Context(), []acp.ContentBlock{acp.Text("active")})
		firstTurn := <-turns
		other.Submit(t.Context(), []acp.ContentBlock{acp.Text("active")})
		otherTurn := <-turns
		a := &Agent{sessions: map[string]*Session{first.id: first, other.id: other}}

		in, client := io.Pipe()
		releaseOutput := make(chan struct{})
		drain := sync.OnceFunc(func() { close(releaseOutput) })
		out := &pausedOutput{release: releaseOutput}
		conn := jsonrpc.NewConn(in, out, slog.New(slog.DiscardHandler))
		methods := jsonrpc.Methods{
			Requests:      map[string]jsonrpc.Handler{acp.MethodSessionPrompt: jsonrpc.Handle(a.prompt)},
			Notifications: map[string]jsonrpc.Handler{acp.MethodSessionCancel: jsonrpc.Handle(a.cancelSession)},
		}
		served := make(chan struct{})
		go func() {
			defer close(served)
			_ = conn.Serve(t.Context(), methods)
		}()
		defer func() {
			drain()
			resumeModels()
			_ = client.Close()
			<-served
			first.Close()
			other.Close()
			_ = conn.Close()
		}()
		// The writer takes the first line and all 256 ordinary slots fill.
		for range 257 {
			if err := conn.Notify("filler", nil); err != nil {
				t.Fatal(err)
			}
		}
		prompt := func(id int, key string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/prompt","params":{"sessionId":%q,"prompt":[{"type":"text","text":"queued"}]%s}}`, id, first.id, key)
		}
		cancel := func(session string) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":%q}}`, session)
		}
		key := `,"idempotencyKey":"retry"`
		frames := []string{prompt(1, ""), prompt(2, key), prompt(3, key), cancel(first.id), prompt(4, key), cancel(other.id)}
		if _, err := fmt.Fprintln(client, strings.Join(frames, "\n")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if firstTurn.Err() == nil || otherTurn.Err() == nil {
			t.Fatalf("reader did not cancel both sessions with output full: first %v, next %v", firstTurn.Err(), otherTurn.Err())
		}
		group := a.prompts.groups[promptIdentity{first.id, "retry"}]
		if len(first.queue) != 1 || group == nil || first.queue[0].reply != group || len(group.waiters) != 1 {
			t.Fatal("retry after cancel did not get its own queue entry and group")
		}

		closed := make(chan struct{})
		go func() { first.Close(); close(closed) }()
		synctest.Wait()
		resumeModels()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close returned before the queued rejections could be sent")
		default:
		}
		drain()
		<-closed
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		rejected := map[int]int{}
		for line := range strings.Lines(out.String()) {
			var msg struct {
				ID    int
				Error *jsonrpc.Error
			}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatal(err)
			}
			if msg.ID != 0 {
				if msg.Error == nil || msg.Error.Code != -32800 {
					t.Fatalf("expected a cancelled prompt: %s", line)
				}
				rejected[msg.ID]++
			}
		}
		for id := 1; id <= 4; id++ {
			if rejected[id] != 1 {
				t.Fatalf("missing or duplicate rejection of %d: %v", id, rejected)
			}
		}
	})
}
