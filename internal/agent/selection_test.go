package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

func TestNewSessionModelPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, fallback string
		saved, want    modelSelection
	}{
		{name: "fresh launcher default", fallback: "r", want: modelSelection{"r", effortDefault}},
		{name: "remembered choice first", fallback: "m", saved: modelSelection{"r", "high"}, want: modelSelection{"r", "high"}},
		{name: "invalid remembered effort", saved: modelSelection{"r", "unsupported"}, want: modelSelection{"r", effortDefault}},
		{name: "plain model drops effort", fallback: "r", saved: modelSelection{"m", "high"}, want: modelSelection{"m", effortDefault}},
		{name: "filtered remembered choice", fallback: "r", saved: modelSelection{"no-tools", "high"}, want: modelSelection{"r", effortDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := startAgentWithConfig(t, t.TempDir(), nil, nil, Config{DefaultModel: tc.fallback}, nil)
			st := c.agent.cfg.Store
			if tc.saved.model != "" {
				if err := st.SetSelection(t.Context(), tc.saved.model, tc.saved.effort); err != nil {
					t.Fatal(err)
				}
			}
			c.initialize()
			var res acp.NewSessionResponse
			c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()}, &res)
			if got := res.ConfigOptions[0]; got.CurrentValue != tc.want.model || !reflect.DeepEqual(got.Options, newCatalog(testModels).choices) {
				t.Fatalf("session advertised %+v, want effective model %q in the tool-capable catalog", got, tc.want.model)
			}
			l, err := st.Acquire(t.Context(), res.SessionID)
			if err != nil || l.Model != tc.want.model || l.Effort != tc.want.effort {
				t.Fatalf("stored effective selection: %+v, %v", l, err)
			}
			m, e, err := st.Selection(t.Context())
			if err != nil || (modelSelection{m, e}) != tc.saved {
				t.Fatalf("new session changed the explicit preference: %q %q, %v", m, e, err)
			}
		})
	}
}

func TestColdResumeModelPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, fallback string
		saved, want    modelSelection
		cancel         bool
	}{
		{name: "saved session before launcher and remembered choice", fallback: "m", saved: modelSelection{"r", "low"}, want: modelSelection{"r", "low"}},
		{name: "legacy blank model", fallback: "r", saved: modelSelection{"", "high"}, want: modelSelection{"r", effortDefault}},
		{name: "invalid saved effort", saved: modelSelection{"r", "unsupported"}, want: modelSelection{"r", effortDefault}},
		{name: "no authorized model", saved: modelSelection{"removed", "high"}},
		{name: "cancel during catalog load", fallback: "r", saved: modelSelection{"removed", "high"}, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acquired := make(chan struct{}, 1)
			releaseCatalog := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(releaseCatalog) })
			var list ListModels
			if tc.cancel {
				list = func(ctx context.Context) ([]llm.Model, error) {
					select {
					case <-releaseCatalog:
						return testModels, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			c := startAgentWithConfig(t, t.TempDir(), nil, list, Config{DefaultModel: tc.fallback}, nil,
				func(st Store) Store { return acquiredStore{st, acquired} })
			defer unblock()
			st := c.agent.cfg.Store.(acquiredStore).Store
			ctx := t.Context()
			sess := store.Session{ID: "saved", Cwd: t.TempDir(), Dirs: []string{t.TempDir()}, Model: tc.saved.model, Effort: tc.saved.effort, Mode: modePlan, State: "running", UpdatedAt: time.Now()}
			history := [][]byte{[]byte(`{"role":"user","content":"saved message"}`)}
			msg := acp.UserMessage{MessageID: "original-id", Content: []acp.ContentBlock{acp.Text("saved message")}}
			entry, err := encodeEntry(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			if err := st.Save(ctx, sess, []store.Row{{Data: history[0]}}, []store.Row{{Data: entry}}, nil); err != nil {
				t.Fatal(err)
			}
			if err := st.Release(ctx, sess.ID); err != nil {
				t.Fatal(err)
			}
			if err := st.SetSelection(ctx, "m", effortDefault); err != nil {
				t.Fatal(err)
			}
			c.initialize()
			params := map[string]any{"sessionId": sess.ID, "cwd": sess.Cwd, "replayFrom": map[string]string{"type": "start"}}
			if tc.want.model == "" {
				var mcpRequests atomic.Int32
				peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mcpRequests.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer peer.Close()
				params["mcpServers"] = []acp.MCPServer{{Type: "http", Name: "observer", URL: peer.URL}}
				if tc.cancel {
					resumed := c.send(acp.MethodSessionResume, params)
					id := c.lastID()
					issue50Wait(t, acquired)
					c.notify("$/cancel_request", map[string]any{"requestId": id})
					c.ok(acp.MethodSessionList, map[string]any{}, nil)
					unblock()
					if r := c.await(resumed); r.Error == nil || r.Error.Code != -32800 {
						t.Fatalf("cancelled cold restore: %+v", r.Error)
					}
					c.agent.jobs.Wait() // Cancellation may reply before the restore worker releases ownership.
				} else if e := c.fails(acp.MethodSessionResume, params, jsonrpc.CodeInternalError); !strings.Contains(e.Message, "ACPLAUSE_DEFAULT_MODEL") {
					t.Fatalf("missing launcher guidance: %+v", e)
				}
				if err := st.CheckOwner(ctx, sess.ID); !errors.Is(err, store.ErrLost) {
					t.Fatalf("failed restore retained ownership: %v", err)
				}
				if len(c.updates) != 0 || mcpRequests.Load() != 0 {
					t.Fatal("failed restore emitted replay or contacted MCP")
				}
			} else {
				var res acp.ResumeSessionResponse
				c.ok(acp.MethodSessionResume, params, &res)
				updates := c.untilIdle(sess.ID)
				var replayed acp.UserMessage
				if len(updates) != 3 || updates[0].Update.Kind != "user_message" || json.Unmarshal(updates[0].Update.Raw, &replayed) != nil || replayed.MessageID != msg.MessageID || updates[1].Update.Kind != "replay_complete" || res.ConfigOptions[0].CurrentValue != tc.want.model {
					t.Fatalf("cold resume configuration %+v and replay %+v", res.ConfigOptions, updates)
				}
				sess.Model, sess.Effort, sess.State, sess.Dirs = tc.want.model, tc.want.effort, "idle", nil
			}
			l, err := st.Acquire(ctx, sess.ID)
			if err != nil || l.Model != sess.Model || l.Effort != sess.Effort || l.State != sess.State || l.Mode != sess.Mode || !reflect.DeepEqual(l.History, history) || !reflect.DeepEqual(l.Transcript, [][]byte{entry}) || !slices.Equal(l.Dirs, sess.Dirs) {
				t.Fatalf("stored session after resume: %+v, %v", l, err)
			}
			m, e, err := st.Selection(ctx)
			if err != nil || m != "m" || e != effortDefault {
				t.Fatalf("resume changed the explicit preference: %q %q, %v", m, e, err)
			}
		})
	}
}
