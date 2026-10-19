package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/llm"
)

// imagePeer exercises real MCP routing without a subprocess or provider API.
func imagePeer(t *testing.T, call func(*http.Request) any) acp.MCPServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			ID     json.RawMessage
			Method string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "image", "inputSchema": map[string]string{"type": "object"}}}}
		case "tools/call":
			result = call(r)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return acp.MCPServer{Type: "http", Name: "fixture", URL: srv.URL}
}

func TestMCPImagesUseRoundModelAndSurviveRecovery(t *testing.T) {
	for _, before := range []string{"vision", "text"} {
		t.Run(before, func(t *testing.T) {
			img := imageBlock(t, testImage(t, 3, 2))
			entered, release := make(chan struct{}), make(chan struct{}, 1)
			defer close(release)
			var calls atomic.Int32
			cfg := imagePeer(t, func(r *http.Request) any {
				calls.Add(1)
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return map[string]any{"content": []any{img}}
			})
			model := make(scriptedModel, 3)
			model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
				for _, tool := range req.Tools {
					if strings.HasPrefix(tool.Function.Name, "mcp_") {
						return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "shot", Function: llm.FunctionCall{Name: tool.Function.Name, Arguments: `{}`}}}}, nil
					}
				}
				t.Error("MCP image tool absent")
				return &llm.Completion{}, nil
			}
			requests := make(chan llm.Request, 2)
			for range 2 {
				model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
					requests <- req
					return &llm.Completion{FinishReason: llm.FinishStop}, nil
				}
			}
			models := []llm.Model{{ID: "vision"}, {ID: "text"}}
			models[0].Architecture.InputModalities = []string{"text", "image"}
			c := startAgent(t, t.TempDir(), model, func(context.Context) ([]llm.Model, error) { return models, nil })
			c.initialize()
			cwd := t.TempDir()
			var session acp.NewSessionResponse
			c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd, MCPServers: []acp.MCPServer{cfg}}, &session)
			sid := session.SessionID
			c.selectModel(sid, before)
			c.ok(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text("screenshot")}}, nil)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("MCP call never arrived")
			}
			after := "text"
			if before == "text" {
				after = "vision"
			}
			c.selectModel(sid, after)
			release <- struct{}{}
			updates := c.untilIdle(sid)
			wantImages := 0
			if before == "vision" {
				wantImages = 1
			}
			checkReplay := func(updates []notification) {
				t.Helper()
				found := false
				for _, u := range updates {
					if u.Update.Kind != "tool_call_update" || u.Update.Status == "pending" || u.Update.Status == "in_progress" {
						continue
					}
					found = true
					var tool acp.ToolCallUpdate
					if err := json.Unmarshal(u.Update.Raw, &tool); err != nil {
						t.Fatal(err)
					}
					if len(tool.Content) != wantImages+1 || (tool.Status == acp.ToolFailed) != (wantImages == 0) || strings.Contains(string(tool.RawOutput), img.Data) {
						t.Fatal("round policy or rawOutput sanitization violated")
					}
				}
				if !found {
					t.Fatal("no final tool update")
				}
			}
			checkReplay(updates)
			if req := <-requests; req.Model != after || imageCount(req.Messages) != 0 || req.Messages[3].ToolCallID != "shot" {
				t.Fatal("request-time image policy did not follow the new model")
			}
			c.ok(acp.MethodSessionClose, acp.CloseSessionRequest{SessionID: sid}, nil)
			c.ok(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: sid, Cwd: cwd, ReplayFrom: &acp.ReplayFrom{Type: acp.ReplayFromStart}}, nil)
			checkReplay(c.untilIdle(sid))
			c.selectModel(sid, "vision")
			c.ok(acp.MethodSessionPrompt, acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.Text("look again")}}, nil)
			c.untilIdle(sid)
			req := <-requests
			if imageCount(req.Messages) != wantImages || calls.Load() != 1 {
				t.Fatal("recovery lost images or replayed a remote call")
			}
			assertToolPairs(t, req.Messages)
		})
	}
}

func TestMCPImageProjectionFailureKeepsBindings(t *testing.T) {
	// An invalid image is a model-visible tool result, not a connection
	// failure that would repeat an operation which may have completed.
	var calls atomic.Int32
	cfg := imagePeer(t, func(*http.Request) any {
		calls.Add(1)
		return map[string]any{"content": []any{map[string]any{"type": "image", "mimeType": "image/png", "data": "invalid"}}}
	})
	s, _ := imageSession(t, noModel(t), 0)
	if err := s.Resume(t.Context(), nil, []acp.MCPServer{cfg}, false); err != nil {
		t.Fatal(err)
	}
	tool := s.mcp.Snapshot()[0]
	prepared, err := prepareMCP(tool, `{}`, true)
	if err != nil {
		t.Fatal(err)
	}
	out := prepared.run(t.Context())
	if !out.Failed || calls.Load() != 1 || len(s.mcp.Snapshot()) != 1 || !strings.Contains(out.Output, "may have completed") {
		t.Fatal("projection error invalidated or repeated the MCP operation")
	}
}

func TestMCPImagesCancellationKeepsSiblingResults(t *testing.T) {
	img := imageBlock(t, testImage(t, 3, 2))
	entered := make(chan struct{})
	var calls atomic.Int32
	cfg := imagePeer(t, func(r *http.Request) any {
		if calls.Add(1) == 2 {
			close(entered)
			<-r.Context().Done()
		}
		return map[string]any{"content": []any{img}}
	})
	model := make(scriptedModel, 2)
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		var calls []llm.ToolCall
		for _, tool := range req.Tools {
			if strings.HasPrefix(tool.Function.Name, "mcp_") {
				for _, id := range []string{"first", "waiting", "skipped"} {
					calls = append(calls, llm.ToolCall{ID: id, Function: llm.FunctionCall{Name: tool.Function.Name, Arguments: `{}`}})
				}
			}
		}
		return &llm.Completion{ToolCalls: calls}, nil
	}
	model <- func(_ context.Context, req llm.Request, _ func(llm.Delta)) (*llm.Completion, error) {
		assertToolPairs(t, req.Messages)
		if imageCount(req.Messages) != 1 || len(req.Messages) != 7 {
			t.Error("cancellation dropped a completed image or a sibling tool result")
		}
		for i, id := range []string{"first", "waiting", "skipped"} {
			if req.Messages[3+i].ToolCallID != id || (len(req.Messages[3+i].Images) > 0) != (i == 0) {
				t.Error("cancelled batch lost tool attribution or retained cancelled pixels")
			}
		}
		return &llm.Completion{FinishReason: llm.FinishStop}, nil
	}
	s, out := imageSession(t, model, 0)
	if err := s.Resume(t.Context(), nil, []acp.MCPServer{cfg}, false); err != nil {
		t.Fatal(err)
	}
	<-out // resume's idle snapshot
	s.Submit(t.Context(), []acp.ContentBlock{acp.Text("screenshots")})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second MCP call never arrived")
	}
	s.Cancel()
	updates := out.untilIdle(t)
	if idle := updates[len(updates)-1].(acp.StateUpdate); idle.StopReason != acp.StopCancelled {
		t.Fatal("cancelled image turn did not end cancelled")
	}
	if calls.Load() != 2 {
		t.Fatal("unstarted sibling was dispatched or a call repeated")
	}
	s.Submit(t.Context(), []acp.ContentBlock{acp.Text("continue")})
	out.untilIdle(t)
}
