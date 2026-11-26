package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/store"
)

type mcpWarningStore struct{ Store }

func (mcpWarningStore) Selection(context.Context) (string, string, error) { return "", "", nil }
func (mcpWarningStore) Create(context.Context, store.Session) error       { return nil }
func (mcpWarningStore) Release(context.Context, string) error             { return nil }

func TestMCPNewWarningOrdersCloseWithFullOutput(t *testing.T) {
	cwd := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		in, client := io.Pipe()
		release := make(chan struct{})
		drain := sync.OnceFunc(func() { close(release) })
		out := &pausedOutput{release: release}
		conn := jsonrpc.NewConn(in, out, slog.New(slog.DiscardHandler))
		a := New(Config{DefaultModel: "m", Store: mcpWarningStore{}}, conn, nil, func(context.Context) ([]llm.Model, error) {
			return testModels, nil
		})
		a.initialized = true
		seen := false
		methods := a.Methods()
		methods.Notifications["test/seen"] = func(context.Context, json.RawMessage) (any, error) {
			seen = true
			return nil, nil
		}
		served := make(chan struct{})
		go func() {
			defer close(served)
			_ = conn.Serve(t.Context(), methods)
		}()
		defer func() {
			drain()
			_ = client.Close()
			<-served
			a.Shutdown()
			_ = conn.Close()
		}()
		// Leave one ordinary slot for the new response. The warning has to
		// wait for the writer without blocking close on the reader.
		for range 256 {
			if err := conn.Notify("filler", nil); err != nil {
				t.Fatal(err)
			}
		}
		send := func(frame string, args ...any) {
			t.Helper()
			if _, err := fmt.Fprintf(client, frame+"\n", args...); err != nil {
				t.Fatal(err)
			}
		}
		send(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":%q,"mcpServers":[{"type":"stdio","name":"missing","command":%q}]}}`,
			cwd, filepath.Join(cwd, "does-not-exist"))
		synctest.Wait()
		var s *Session
		for _, s = range a.sessions {
		}
		if s == nil {
			t.Fatal("new session has not reached publication")
		}
		if !s.mu.TryLock() {
			t.Fatal("the warning holds the session lock while waiting for capacity")
		}
		s.mu.Unlock()
		sid := s.id
		send(`{"jsonrpc":"2.0","id":2,"method":"session/resume","params":{"sessionId":%q,"cwd":%q,"replayFrom":{"type":"start"}}}`, sid, cwd)
		send(`{"jsonrpc":"2.0","id":3,"method":"session/close","params":{"sessionId":%q}}`, sid)
		send(`{"jsonrpc":"2.0","method":"test/seen"}`)
		synctest.Wait()
		if !seen {
			t.Fatal("waiting to enqueue the warning blocked the reader at close")
		}
		drain()
		a.jobs.Wait()
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		newResponse, warning, closeResponse := -1, -1, -1
		var replayErr *jsonrpc.Error
		for i, line := range strings.Split(out.String(), "\n") {
			var frame struct {
				ID     int
				Error  *jsonrpc.Error
				Params struct {
					Update acp.Notice
				}
			}
			if line == "" {
				continue
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatal(err)
			}
			switch frame.ID {
			case 1:
				newResponse = i
			case 2:
				replayErr = frame.Error
			case 3:
				if frame.Error != nil {
					t.Fatalf("close failed: %v", frame.Error)
				}
				closeResponse = i
			}
			if frame.Params.Update.Title == "MCP servers unavailable" {
				warning = i
			}
		}
		if replayErr == nil || replayErr.Code != replayConflict {
			t.Fatalf("cursor entered while setup still owed the warning: %v", replayErr)
		}
		if newResponse < 0 || warning <= newResponse || closeResponse <= warning {
			t.Fatalf("wrong output order: new=%d warning=%d close=%d", newResponse, warning, closeResponse)
		}
	})
}

func TestMCPPermissionObservesRevocationBeforeCancellationRelay(t *testing.T) {
	turn, stopTurn := context.WithCancel(t.Context())
	defer stopTurn()
	binding, revoke := context.WithCancel(t.Context())
	defer revoke()
	wait, cancel := context.WithCancel(turn)
	defer cancel()
	permission := mcpPermissionContext{Context: wait, binding: binding, cancel: cancel}
	if err := permission.Err(); err != nil {
		t.Fatal(err)
	}
	// The worker's AfterFunc has not relayed the revocation yet.
	revoke()
	if err := permission.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("permission delivery accepted a revoked binding: %v", err)
	}
	select {
	case <-permission.Done():
	default:
		t.Fatal("Err reported cancellation before Done")
	}
	if turn.Err() != nil {
		t.Fatal("binding revocation cancelled the turn")
	}
}

func TestMCPSetupPublishesPartialCatalogAndLiveWarnings(t *testing.T) {
	good := imagePeer(t, func(*http.Request) any { return nil })
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream-private-error", http.StatusServiceUnavailable)
	}))
	t.Cleanup(peer.Close)
	bad := acp.MCPServer{Type: "http", Name: "private-name", URL: peer.URL + "/private-path", Headers: []acp.MCPVariable{{Name: "Authorization", Value: "Bearer private-credential"}}}
	c := startAgent(t, t.TempDir(), make(scriptedModel), nil)
	c.initialize()
	cwd := t.TempDir()
	var created acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd, MCPServers: []acp.MCPServer{bad, good}}, &created)
	sid := created.SessionID
	assertNotice := func(title, description string) {
		t.Helper()
		n := c.waitFor(sid, func(n notification) bool { return n.Update.Kind == "notice" })
		var notice acp.Notice
		if err := json.Unmarshal(n.Update.Raw, &notice); err != nil || notice != (acp.Notice{Title: title, Severity: "warning", Description: description}) {
			t.Fatalf("setup notice: %+v, %v", notice, err)
		}
	}
	assertNotice("MCP setup incomplete", "Server 1 (http) could not initialize connection.\nConnected MCP servers remain available.")
	s, err := c.agent.session(sid)
	if err != nil {
		t.Fatal(err)
	}
	tools := s.mcp.Snapshot()
	if len(tools) != 1 || tools[0].Server != good.Name {
		t.Fatalf("wrong subset after new: %+v", tools)
	}
	bad.Name = good.Name
	c.ok(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: sid, Cwd: cwd, MCPServers: []acp.MCPServer{bad}}, nil)
	if got := c.untilIdle(sid); len(got) != 2 || got[0].Update.Kind != "session_info_update" || got[1].Update.Kind != "state_update" {
		t.Fatalf("wrong cursorless synchronization: %+v", got)
	}
	assertNotice("MCP servers unavailable", "Server 1 (http) could not initialize connection.\nThe session will use built-in tools.")
	if len(s.mcp.Snapshot()) != 0 || tools[0].Context().Err() == nil {
		t.Fatal("failed replacement preserved the previous binding")
	}
	c.ok(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: sid, Cwd: cwd, ReplayFrom: &acp.ReplayFrom{Type: acp.ReplayFromStart}}, nil)
	got := c.untilIdle(sid)
	if len(got) != 3 || got[0].Update.Kind != "replay_complete" || got[1].Update.Kind != "session_info_update" || got[2].Update.Kind != "state_update" {
		t.Fatalf("old warnings entered replay: %+v", got)
	}
}

func gatedMCPReplacement(t *testing.T) (*acpClient, *Session, acp.MCPServer, <-chan struct{}, func(), string, string) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	badPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(badPeer.Close)
	t.Cleanup(unblock)
	good := imagePeer(t, func(*http.Request) any { return nil })
	bad := acp.MCPServer{Type: "http", Name: good.Name, URL: badPeer.URL}
	dir, cwd := t.TempDir(), t.TempDir()
	c := startAgent(t, dir, nil, nil)
	c.initialize()
	var created acp.NewSessionResponse
	c.ok(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd, MCPServers: []acp.MCPServer{good}}, &created)
	s, err := c.agent.session(created.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return c, s, bad, entered, unblock, dir, cwd
}

func TestMCPWarmResumeCannotPublishAfterLeaseTakeover(t *testing.T) {
	c, s, bad, entered, unblock, dir, cwd := gatedMCPReplacement(t)
	defer unblock()
	old := s.mcp.Snapshot()[0]
	response := c.send(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: s.id, Cwd: cwd, MCPServers: []acp.MCPServer{bad}})
	issue50Wait(t, entered)
	other, err := store.Open(dir, "review-new-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Acquire(t.Context(), s.id); err != nil {
		t.Fatal(err)
	}
	if err := other.CheckOwner(t.Context(), s.id); err != nil {
		t.Fatal(err)
	}
	unblock()
	r := c.await(response)
	if r.Error == nil || r.Error.Code != jsonrpc.CodeInternalError || r.Error.Message != store.ErrLost.Error() {
		t.Fatalf("resume after durable lease takeover: %+v", r)
	}
	c.agent.jobs.Wait()
	if old.Context().Err() == nil || len(s.mcp.Snapshot()) != 0 {
		t.Fatal("lost generation still has live MCP bindings")
	}
	if err := other.CheckOwner(t.Context(), s.id); err != nil {
		t.Fatalf("retiring the old generation released the new owner's lease: %v", err)
	}
}

func TestMCPReplayOverflowPreservesPreviousCatalog(t *testing.T) {
	c, s, bad, entered, unblock, _, cwd := gatedMCPReplacement(t)
	defer unblock()
	old := s.mcp.Snapshot()[0]
	response := c.send(acp.MethodSessionResume, acp.ResumeSessionRequest{SessionID: s.id, Cwd: cwd, MCPServers: []acp.MCPServer{bad}, ReplayFrom: &acp.ReplayFrom{Type: acp.ReplayFromStart}})
	issue50Wait(t, entered)
	s.mu.Lock()
	for i := range maxReplayFrames {
		s.send(acp.Notice{Title: fmt.Sprint(i), Severity: "info"})
	}
	s.mu.Unlock()
	unblock()
	r := c.await(response)
	if r.Error == nil || r.Error.Code != jsonrpc.CodeInternalError {
		t.Fatalf("expected replay overflow: %+v", r)
	}
	if old.Context().Err() != nil {
		t.Fatalf("resume rejected with %v, but previous binding=%v and active tools=%d", r.Error, old.Context().Err(), len(s.mcp.Snapshot()))
	}
}
