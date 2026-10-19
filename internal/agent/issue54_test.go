package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

// issue54Client signals after the real insertion path queues its user message.
type issue54Client struct {
	Client
	notified chan struct{}
}

func (c issue54Client) Notify(method string, params any) error {
	err := c.Client.Notify(method, params)
	if n, ok := params.(acp.SessionNotification); ok {
		if _, ok := n.Update.(acp.UserMessage); ok {
			close(c.notified)
		}
	}
	return err
}

func TestIssue54EarlyPromptResponsePrecedesUserMessage(t *testing.T) {
	model := make(scriptedModel, 1)
	model.reply("done", llm.FinishStop, nil)
	s, _ := testSession(t, model)
	s.saves.Wait() // Finish the helper's configuration save before replacing output.
	var output bytes.Buffer
	conn := jsonrpc.NewConn(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"session/prompt"}`+"\n"), &output, slog.New(slog.DiscardHandler))
	t.Cleanup(func() {
		s.Close()
		_ = conn.Close()
	})
	notified := make(chan struct{})
	s.deps.out = issue54Client{Client: conn, notified: notified}
	methods := jsonrpc.Methods{
		acp.MethodSessionPrompt: func(ctx context.Context, _ json.RawMessage) (any, error) {
			reply := s.Submit(ctx, []acp.ContentBlock{acp.Text("early worker")})
			// Hold the handler until Session.insert has called Resolve and
			// sent user_message. No production timing hook is needed.
			select {
			case <-notified:
			case <-time.After(2 * time.Second):
				t.Error("insertion did not publish user_message before handler return")
			}
			return reply, nil
		},
	}
	if err := conn.Serve(context.Background(), methods); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Decode the actual NDJSON stream, before separating responses from updates.
	dec := json.NewDecoder(&output)
	var first struct {
		ID     int                `json:"id"`
		Method string             `json:"method"`
		Result acp.PromptResponse `json:"result"`
	}
	if err := dec.Decode(&first); err != nil {
		t.Fatal(err)
	}
	if first.ID != 1 || first.Method != "" || first.Result.MessageID == "" {
		t.Fatalf("first wire message = %+v; want prompt response before user_message", first)
	}
	var second struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				acp.UserMessage
				Kind string `json:"sessionUpdate"`
			} `json:"update"`
		} `json:"params"`
	}
	if err := dec.Decode(&second); err != nil {
		t.Fatal(err)
	}
	if second.Method != acp.MethodSessionUpdate || second.Params.SessionID != s.id || second.Params.Update.Kind != "user_message" || second.Params.Update.MessageID != first.Result.MessageID {
		t.Fatalf("next wire message = %+v; want the acknowledged user_message", second)
	}
}
