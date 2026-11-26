package jsonrpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestInvalidRequestEnvelopePreservesOnlyValidIDs(t *testing.T) {
	calls := 0
	p := newPeer(t, map[string]Handler{"count": func(context.Context, json.RawMessage) (any, error) {
		calls++
		return calls, nil
	}})
	for _, tc := range []struct{ line, id string }{
		{`{"jsonrpc":"2.0","id":null,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":1.5,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":9007199254740992.5,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":1e-1000,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":10e9223372036854775807,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":1e9223372036854775808,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":true,"method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","method":"count"}`, "null"},
		{`{"jsonrpc":"2.0","id":"known","method":42}`, `"known"`},
		{`{"JSONRPC":"2.0","id":"known","method":"count"}`, `"known"`},
		{`{"jsonrpc":"2.0","id":"known","METHOD":"count"}`, `"known"`},
		{`{"jsonrpc":2,"id":"known","method":"count"}`, `"known"`},
		{`{"jsonrpc":"2.0","id":"known","method":"count","result":{}}`, `"known"`},
		{`{"jsonrpc":"2.0","id":"known","method":"count","params":"scalar"}`, `"known"`},
		{`{"jsonrpc":"2.0","id":"known","method":"$/cancel_request","params":{"requestId":1}}`, `"known"`},
	} {
		p.send(tc.line)
		var got response
		line := p.recv()
		if err := json.Unmarshal([]byte(line), &got); err != nil || got.JSONRPC != Version || string(got.ID) != tc.id ||
			got.Error == nil || got.Error.Code != CodeInvalidRequest || got.Result != nil {
			t.Fatalf("for %s: got %s (%v), want Invalid Request with id %s", tc.line, line, err, tc.id)
		}
	}
	p.send(`{"jsonrpc":"2.0","id":0,"method":"count","_meta":{"extension":true}}`)
	p.expect(`{"jsonrpc":"2.0","id":0,"result":1}`)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"count","params":null}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"result":2}`)
}

func TestMalformedResponsesDoNotClaimPendingCall(t *testing.T) {
	p := newPeer(t, map[string]Handler{"echo": echo})
	var got struct{ N int }
	done := make(chan error, 1)
	go func() { done <- p.conn.CallPermission(t.Context(), "ask", nil, &got) }()
	p.expect(`{"jsonrpc":"2.0","id":1,"method":"ask"}`)
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"unknown ID"}}`,
		`{"jsonrpc":"2.0","id":null,"result":{"n":99}}`,
		`{"jsonrpc":"2.0","id":1.5,"result":{"n":99}}`,
		`{"jsonrpc":"2.0","id":"1","result":{"n":99}}`,
		`{"jsonrpc":"1.0","id":1,"result":{"n":99}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"n":99},"error":{"code":-32600,"message":"both"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603}}`,
		`{"jsonrpc":"2.0","id":1,"error":[]}`,
		`{"jsonrpc":"2.0","id":1,"error":{"message":"no code"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"CODE":-32603,"MESSAGE":"malformed"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32603.5,"message":"fractional"}}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
		`{"jsonrpc":"2.0","result":{"n":99}}`,
	} {
		p.send(line)
	}
	p.send(`{"jsonrpc":"2.0","id":"fence","method":"echo","params":{}}`)
	p.expect(`{"jsonrpc":"2.0","id":"fence","result":{}}`)
	p.send(`{"jsonrpc":"2.0","id":1.00,"result":{"n":7}}`)
	select {
	case err := <-done:
		if err != nil || got.N != 7 {
			t.Fatalf("pending call got %+v, %v", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call never received the valid response")
	}
}

func TestCancelMatchesOnlyTheSameRequestIDValue(t *testing.T) {
	admitted := make(chan context.Context, 1)
	p := newPeer(t, map[string]Handler{
		"echo": echo,
		"wait": func(ctx context.Context, _ json.RawMessage) (any, error) {
			reply := NewReply(ctx)
			context.AfterFunc(ctx, func() { reply.Reject(NewError(-32800, "cancelled")) })
			admitted <- ctx
			return reply, nil
		},
	})
	for _, tc := range []struct{ id, otherType, sameValue string }{
		{`"7"`, `7`, `"\u0037"`},
		{`7.0`, `"7"`, `7e0`},
	} {
		p.send(`{"jsonrpc":"2.0","id":` + tc.id + `,"method":"wait"}`)
		ctx := <-admitted
		p.send(`{"jsonrpc":"2.0","id":"wrong-form","method":"$/cancel_request","params":{"requestId":` + tc.sameValue + `}}`)
		p.expect(`{"jsonrpc":"2.0","id":"wrong-form","error":{"code":-32600,"message":"method $/cancel_request is a notification"}}`)
		for _, id := range []string{"null", "7.1", tc.otherType} {
			p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + id + `}}`)
		}
		p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":8,"REQUESTID":` + tc.sameValue + `}}`)
		p.send(`{"jsonrpc":"2.0","id":"fence","method":"echo","params":{}}`)
		p.expect(`{"jsonrpc":"2.0","id":"fence","result":{}}`)
		if err := ctx.Err(); err != nil {
			t.Fatalf("invalid cancellation of %s reached handler: %v", tc.id, err)
		}
		p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + tc.sameValue + `}}`)
		want := `{"jsonrpc":"2.0","id":` + tc.id + `,"error":{"code":-32800,"message":"cancelled"}}`
		if got := p.recv(); got != want {
			t.Fatalf("cancel response = %s, want %s", got, want)
		}
	}
}
