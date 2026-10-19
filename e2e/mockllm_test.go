package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Models offered by the mock: one that reasons, one that does not, and one
// without tool support that the agent must hide.
const modelsJSON = `{"data":[
	{"id":"test/model","name":"Test Model","context_length":64000,"supported_parameters":["tools","reasoning"]},
	{"id":"test/plain","name":"Test Plain","context_length":128000,"supported_parameters":["tools"]},
	{"id":"test/notools","name":"No Tools","context_length":3000,"supported_parameters":["temperature"]}
]}`

// mockLLM is a scripted OpenAI-compatible server. Each chat request is
// answered by the next queued script.
type mockLLM struct {
	t      *testing.T
	srv    *httptest.Server
	models string

	mu       sync.Mutex
	scripts  []script
	requests []chatRequest
	arrived  chan chatRequest
}

type script func(s *stream)

type chatRequest struct {
	MaxTokens uint64           `json:"max_tokens"`
	Model     string           `json:"model"`
	Stream    bool             `json:"stream"`
	Messages  []map[string]any `json:"messages"`
	Tools     []map[string]any `json:"tools"`
	Reasoning *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func newMockLLM(t *testing.T, catalog ...string) *mockLLM {
	m := &mockLLM{t: t, models: modelsJSON, arrived: make(chan chatRequest, 32)}
	if len(catalog) > 0 {
		m.models = catalog[0]
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// enqueue adds scripts for the next chat requests, in order.
func (m *mockLLM) enqueue(scripts ...script) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = append(m.scripts, scripts...)
}

// request returns the n-th chat request received, waiting for it.
func (m *mockLLM) request(n int) chatRequest {
	m.t.Helper()
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		if n < len(m.requests) {
			r := m.requests[n]
			m.mu.Unlock()
			return r
		}
		m.mu.Unlock()
		select {
		case <-m.arrived:
		case <-deadline:
			m.t.Fatalf("chat request %d never arrived", n)
		}
	}
}

func (m *mockLLM) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockLLM) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-key" {
		http.Error(w, `{"error":{"code":401,"message":"missing key"}}`, http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/models":
		fmt.Fprint(w, m.models)
	case "/chat/completions":
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			m.t.Errorf("bad chat request: %v", err)
		}
		m.mu.Lock()
		m.requests = append(m.requests, req)
		var next script
		if len(m.scripts) > 0 {
			next, m.scripts = m.scripts[0], m.scripts[1:]
		}
		m.mu.Unlock()
		select {
		case m.arrived <- req:
		default: // a waiter is already due to wake up
		}
		if next == nil {
			m.t.Errorf("unexpected chat request #%d", m.requestCount())
			http.Error(w, "no script", http.StatusInternalServerError)
			return
		}
		next(&stream{w: w, r: r, req: req})
	default:
		http.NotFound(w, r)
	}
}

// stream writes one SSE response.
type stream struct {
	req chatRequest
	w   http.ResponseWriter
	r   *http.Request
}

func (s *stream) raw(event string) {
	s.w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(s.w, event)
	s.w.(http.Flusher).Flush()
}

func (s *stream) data(v any) {
	b, _ := json.Marshal(v)
	s.raw("data: " + string(b) + "\n\n")
}

func (s *stream) keepalive() { s.raw(": OPENROUTER PROCESSING\n\n") }
func (s *stream) done()      { s.raw("data: [DONE]\n\n") }

func (s *stream) delta(delta map[string]any) {
	s.data(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
}

func (s *stream) text(t string)      { s.delta(map[string]any{"content": t}) }
func (s *stream) reasoning(t string) { s.delta(map[string]any{"reasoning": t}) }

// toolCall streams a tool call with its arguments split in fragments.
func (s *stream) toolCall(index int, id, name string, argFragments ...string) {
	s.delta(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": id, "type": "function",
		"function": map[string]any{"name": name, "arguments": ""},
	}}})
	for _, f := range argFragments {
		s.delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "function": map[string]any{"arguments": f},
		}}})
	}
}

// finish ends the response with a usage chunk and [DONE].
func (s *stream) finish(reason string) {
	s.data(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ""}, "finish_reason": reason}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "cost": 0.001},
	})
	s.done()
}

// reply answers with text, split into the given chunks.
func reply(chunks ...string) script {
	return func(s *stream) {
		s.keepalive()
		for _, c := range chunks {
			s.text(c)
		}
		s.finish("stop")
	}
}

// callTool answers with one tool call, its arguments split at each "|".
func callTool(name, args string) script {
	return func(s *stream) {
		s.toolCall(0, "call_"+name, name, strings.Split(args, "|")...)
		s.finish("tool_calls")
	}
}

// waitFor answers with a first chunk, then waits for release before
// finishing, or for the agent to abort.
func waitFor(release <-chan struct{}, aborted chan<- struct{}) script {
	return func(s *stream) {
		s.text("working")
		select {
		case <-release:
			s.finish("stop")
		case <-s.r.Context().Done():
			if aborted != nil {
				close(aborted)
			}
		}
	}
}
