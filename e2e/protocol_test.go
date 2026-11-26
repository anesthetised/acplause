package e2e

import (
	"encoding/json"
	"testing"
)

func TestInitialize(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	info := map[string]any{"name": "e2e", "version": "1"}

	// Session methods need initialize first.
	a.fails("session/new", map[string]any{"cwd": t.TempDir()}, -32600)

	e := a.fails("initialize", map[string]any{"protocolVersion": 1, "info": info}, -32022)
	assertJSON(t, e.Data, `{"requested":1,"supported":[2]}`)
	a.fails("initialize", map[string]any{"info": info}, -32602)
	a.fails("initialize", map[string]any{"protocolVersion": "2", "info": info}, -32602)
	a.fails("session/new", map[string]any{"cwd": t.TempDir()}, -32600)

	// A newer client gets v2; unknown fields and _meta are ignored.
	var res struct {
		ProtocolVersion int             `json:"protocolVersion"`
		Info            map[string]any  `json:"info"`
		Capabilities    json.RawMessage `json:"capabilities"`
		AuthMethods     json.RawMessage `json:"authMethods"`
	}
	a.ok("initialize", map[string]any{
		"protocolVersion": 3,
		"info":            info,
		"capabilities":    map[string]any{"_meta": map[string]any{"x.dev/flag": true}, "futureCapability": map[string]any{}},
		"_meta":           map[string]any{"traceparent": "00-1-2-01"},
	}, &res)
	if res.ProtocolVersion != 2 || res.Info["name"] != "acplause" || res.Info["version"] != testVersion || res.AuthMethods != nil {
		t.Fatalf("unexpected initialize result: %+v", res)
	}
	assertJSON(t, res.Capabilities, `{"session":{"mcp":{"stdio":{},"http":{}},"prompt":{"embeddedContext":{},"image":{}},"additionalDirectories":{},"delete":{}}}`)
}

func TestJSONRPCErrors(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	a.initialize()

	a.send(`{"jsonrpc":"2.0","id":1,"method":`)
	if m := a.take(func(m *message) bool { return m.Error != nil }); m.Error.Code != -32700 || string(m.ID) != "null" {
		t.Fatalf("parse error: %+v", m)
	}
	a.send(`{"jsonrpc":"2.0","id":"str-id","method":"_unknown/method","params":{}}`)
	if m := a.response(`"str-id"`); m.Error == nil || m.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", m)
	}
	a.send(`{"jsonrpc":"2.0","method":"_vendor/notice","params":{}}`)
	a.expectQuiet(func(*message) bool { return true })
}

func TestRejectsArraysAndContinuesServing(t *testing.T) {
	t.Parallel()
	a := start(t, newMockLLM(t))
	const notice = `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"missing"}}`
	const init = `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":2,"info":{"name":"e2e","version":"1"}}}`
	for _, line := range []string{`[]`, `[` + notice + `,` + notice + `]`, `[` + init + `,` + notice + `,42]`} {
		a.send(line)
		m := a.take(func(*message) bool { return true })
		if string(m.ID) != "null" || m.Error == nil || m.Error.Code != -32600 {
			t.Fatalf("response to %s: %+v", line, m)
		}
	}
	// The embedded initialize must have left the connection uninitialized.
	a.fails("session/new", map[string]any{"cwd": t.TempDir()}, -32600)
	a.initialize()
	if len(a.pending) != 0 {
		t.Fatalf("extra output: %s", a.describePending())
	}
}

func assertJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("not JSON: %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("got  %s\nwant %s", gb, wb)
	}
}
