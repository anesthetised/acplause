package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{
		BaseURL:    srv.URL,
		APIKey:     "sk-test",
		Header:     http.Header{"X-Openrouter-Title": {"acplause"}},
		RetryDelay: time.Millisecond,
	})
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		fmt.Fprint(w, e)
		w.(http.Flusher).Flush()
	}
}

func data(v string) string { return "data: " + v + "\n\n" }

func collect(t *testing.T, c *Client) (*Completion, []Delta, error) {
	t.Helper()
	var deltas []Delta
	comp, err := c.Chat(context.Background(), Request{Model: "m"}, func(d Delta) { deltas = append(deltas, d) })
	return comp, deltas, err
}

func TestChatOutputBudget(t *testing.T) {
	for _, limit := range []uint64{0, 4096} {
		body, err := json.Marshal(newChatBody(Request{MaxTokens: limit}))
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		if limit == 0 && wire["max_tokens"] != nil || limit != 0 && wire["max_tokens"] != float64(limit) {
			t.Fatalf("max_tokens=%d: %s", limit, body)
		}
	}
}

func TestChatStreamsTextReasoningAndUsage(t *testing.T) {
	var body map[string]any
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("X-OpenRouter-Title") != "acplause" {
			t.Errorf("missing headers: %v", r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sse(w,
			": OPENROUTER PROCESSING\n\n",
			data(`{"choices":[{"delta":{"reasoning":"think","reasoning_details":[{"type":"reasoning.text","text":"think","index":0}]}}]}`),
			data(`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","text":"ing","signature":"sig","index":0}],"reasoning":"ing"}}]}`),
			data(`{"choices":[{"delta":{"content":"Hel"}}]}`),
			": OPENROUTER PROCESSING\n\n",
			data(`{"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`),
			data(`{"choices":[{"delta":{"content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost":0.25,"completion_tokens_details":{"reasoning_tokens":2}}}`),
			data(`[DONE]`),
		)
	})

	comp, deltas, err := collect(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if comp.Content != "Hello" || comp.Reasoning != "thinking" || comp.FinishReason != FinishStop {
		t.Fatalf("unexpected completion: %+v", comp)
	}
	wantDetails := []ReasoningDetail{{"type": "reasoning.text", "text": "thinking", "signature": "sig", "index": 0.0}}
	if !reflect.DeepEqual(comp.ReasoningDetails, wantDetails) {
		t.Fatalf("details: got %v, want %v", comp.ReasoningDetails, wantDetails)
	}
	if u := comp.Usage; u == nil || u.PromptTokens != 10 || u.CompletionTokens != 5 || u.Cost != 0.25 || u.CompletionDetails.ReasoningTokens != 2 {
		t.Fatalf("unexpected usage: %+v", comp.Usage)
	}
	wantDeltas := []Delta{{DeltaReasoning, "think"}, {DeltaReasoning, "ing"}, {DeltaText, "Hel"}, {DeltaText, "lo"}}
	if !reflect.DeepEqual(deltas, wantDeltas) {
		t.Fatalf("deltas: got %v, want %v", deltas, wantDeltas)
	}
	if body["stream"] != true || body["model"] != "m" || body["usage"] != nil || body["reasoning"] != nil {
		t.Fatalf("unexpected request body: %v", body)
	}
}

func TestChatAssemblesToolCallFragments(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go.mod\"}"}},{"index":1,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`),
			data(`{"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"name":"list_dir","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`),
			data(`[DONE]`),
		)
	})
	comp, _, err := collect(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(comp.ToolCalls) != 3 {
		t.Fatalf("got %d tool calls: %+v", len(comp.ToolCalls), comp.ToolCalls)
	}
	want := []FunctionCall{{"read_file", `{"path":"go.mod"}`}, {"bash", `{"command":"ls"}`}, {"list_dir", `{`}}
	for i, call := range comp.ToolCalls {
		if call.Function != want[i] || call.Type != "function" {
			t.Errorf("call %d: got %+v, want %+v", i, call.Function, want[i])
		}
	}
	if comp.ToolCalls[0].ID != "call_a" || !strings.HasPrefix(comp.ToolCalls[2].ID, "call_") {
		t.Fatalf("unexpected ids: %q, %q", comp.ToolCalls[0].ID, comp.ToolCalls[2].ID)
	}
	if msg := comp.Message(); msg.Role != RoleAssistant || len(msg.ToolCalls) != 3 {
		t.Fatalf("unexpected history message: %+v", msg)
	}
}

func TestChatErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		kind    ErrorKind
		message string
	}{
		{
			name: "mid-stream provider error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				sse(w,
					data(`{"choices":[{"delta":{"content":"par"}}]}`),
					data(`{"error":{"code":"server_error","message":"Provider disconnected unexpectedly"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}`),
				)
			},
			kind:    KindProvider,
			message: "server_error: Provider disconnected unexpectedly",
		},
		{
			name: "truncated stream",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				sse(w, data(`{"choices":[{"delta":{"content":"par"}}]}`))
			},
			kind:    KindTruncated,
			message: "stream ended before [DONE]",
		},
		{
			name: "malformed chunk",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				sse(w, data(`{"choices":`), data(`[DONE]`))
			},
			kind: KindProtocol,
		},
		{
			name: "auth",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":{"code":401,"message":"No auth credentials found"}}`, http.StatusUnauthorized)
			},
			kind:    KindAuth,
			message: "No auth credentials found",
		},
		{
			name: "out of credits",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "Insufficient credits", http.StatusPaymentRequired)
			},
			kind:    KindPaymentRequired,
			message: "Insufficient credits",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := collect(t, serve(t, tt.handler))
			e, ok := errors.AsType[*Error](err)
			if !ok {
				t.Fatalf("got %v, want *Error", err)
			}
			if e.Kind != tt.kind || (tt.message != "" && e.Message != tt.message) {
				t.Fatalf("got %+v, want kind %s and message %q", e, tt.kind, tt.message)
			}
		})
	}
}

func TestChatRetriesTransientStatusesBeforeTheStream(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case 2:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			sse(w, data(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), data(`[DONE]`))
		}
	})
	comp, _, err := collect(t, c)
	if err != nil || comp.Content != "ok" || calls.Load() != 3 {
		t.Fatalf("got %+v, %v after %d calls", comp, err, calls.Load())
	}
}

func TestChatGivesUpAfterRetries(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	_, _, err := collect(t, c)
	if e, ok := errors.AsType[*Error](err); !ok || e.Kind != KindServer || calls.Load() != 4 {
		t.Fatalf("got %v after %d calls, want server error after 4", err, calls.Load())
	}
}

func TestChatDoesNotRetryPermanentErrors(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "no such model", http.StatusBadRequest)
	})
	if _, _, err := collect(t, c); err == nil || calls.Load() != 1 {
		t.Fatalf("got %v after %d calls, want one failed call", err, calls.Load())
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { target.Add(1) }))
	t.Cleanup(other.Close)
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	_, _, err := collect(t, c)
	e, ok := errors.AsType[*Error](err)
	if !ok || e.Status != http.StatusTemporaryRedirect || e.Retryable() || !strings.Contains(e.Message, "redirect") {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Models(context.Background()); err == nil {
		t.Fatal("models followed the redirect")
	}
	if calls.Load() != 2 || target.Load() != 0 {
		t.Fatalf("calls=%d target=%d, want 2 and 0", calls.Load(), target.Load())
	}
}

func TestChatCancellationAbortsTheStream(t *testing.T) {
	aborted := make(chan struct{})
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, data(`{"choices":[{"delta":{"content":"hi"}}]}`))
		<-r.Context().Done()
		close(aborted)
	})
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.Chat(ctx, Request{Model: "m"}, func(Delta) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not see the request aborted")
	}
}

func TestChatSendsReasoningEffort(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"reasoning":{"effort":"high"}`) {
			t.Errorf("missing reasoning effort: %s", b)
		}
		sse(w, data(`[DONE]`))
	})
	if _, err := c.Chat(context.Background(), Request{Model: "m", ReasoningEffort: "high"}, func(Delta) {}); err != nil {
		t.Fatal(err)
	}
}

func TestModels(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"a/x","name":"X","context_length":8192,"top_provider":{"max_completion_tokens":4096},"supported_parameters":["tools","reasoning"]},{"id":"b/y","name":"Y","context_length":null,"top_provider":{"max_completion_tokens":null},"supported_parameters":["temperature"]},{"id":"c/z","name":"Z"}]}`)
	})
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].ContextLength != 8192 || models[1].ContextLength != 0 || models[0].TopProvider.MaxCompletionTokens != 4096 || models[1].TopProvider.MaxCompletionTokens != 0 || models[2].TopProvider.MaxCompletionTokens != 0 {
		t.Fatalf("unexpected models: %+v", models)
	}
	if !models[0].Supports("reasoning") || models[1].Supports("tools") || !models[2].Supports("tools") {
		t.Fatalf("unexpected Supports results")
	}
}
