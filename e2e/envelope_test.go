package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidJSONRPCEnvelopesDoNotDispatch(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	const params = `"params":{"protocolVersion":2,"info":{"name":"e2e","version":"1"}}`
	for _, id := range []string{`"id":null,`, `"id":1.25,`, ""} {
		a.send(`{"jsonrpc":"2.0",` + id + `"method":"initialize",` + params + `}`)
		got := a.take(func(*message) bool { return true })
		if string(got.ID) != "null" || got.Error == nil || got.Error.Code != -32600 || got.Result != nil {
			t.Fatalf("invalid initialize (%s) returned %+v", id, got)
		}
	}
	// Those envelopes must have left the connection uninitialized.
	a.fails("session/list", map[string]any{}, -32600)
	a.initialize()

	for _, tc := range []struct{ id, params string }{
		{`"list-001"`, "null"},
		{"9007199254740993", "{}"},
		{"1.2e2", "{}"},
	} {
		a.mu.Lock()
		a.methods[tc.id] = "session/list"
		a.mu.Unlock()
		a.send(`{"jsonrpc":"2.0","id":` + tc.id + `,"method":"session/list","params":` + tc.params + `}`)
		got := a.take(func(*message) bool { return true })
		if string(got.ID) != tc.id || got.Error != nil || got.Result == nil {
			t.Fatalf("session/list with %s returned %+v", tc.id, got)
		}
	}
}

func TestWrongEnvelopeKeepsPermissionPending(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t)
	llm.enqueue(callTool("write_file", `{"path":"created","content":"approved"}`), reply("done"))
	a, sid, cwd := started(t, llm, askingUser)
	a.accepted(a.prompt(sid, "write file"))
	req := a.permissionRequest()

	a.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":"wrong-form","method":"session/cancel","params":{"sessionId":%q}}`, sid))
	if got := a.response(`"wrong-form"`); got.Error == nil || got.Error.Code != -32600 || got.Result != nil {
		t.Fatalf("session/cancel request returned %+v", got)
	}
	a.send(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"unknown ID"}}`)
	a.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s.5,"error":{"code":-32603,"message":"fractional ID"}}`, req.ID))
	a.send(fmt.Sprintf(`{"jsonrpc":"1.0","id":%s,"error":{"code":-32603,"message":"wrong version"}}`, req.ID))
	a.answer(req, selected(optionOfKind(t, req, "allow_once")))
	updates := a.untilIdle(sid)
	if got := updates[len(updates)-1].StopReason; got != "end_turn" {
		t.Fatalf("turn ended with %q, updates %v", got, kinds(updates))
	}
	if contents, err := os.ReadFile(filepath.Join(cwd, "created")); err != nil || string(contents) != "approved" {
		t.Fatalf("write after valid permission response: contents %q, error %v", contents, err)
	}
}
