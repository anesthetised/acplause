package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// peer drives a Conn over in-memory pipes.
type peer struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan string
	conn  *Conn
}

func newPeer(t *testing.T, methods Methods) *peer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	conn := NewConn(inR, outW, slog.New(slog.DiscardHandler))
	p := &peer{t: t, in: inW, lines: make(chan string, 16), conn: conn}

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = conn.Serve(context.Background(), methods)
	}()
	go func() {
		defer close(p.lines)
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		<-served
		_ = conn.Close()
		_ = outW.Close()
	})
	return p
}

func (p *peer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		p.t.Fatalf("send: %v", err)
	}
}

func (p *peer) recv() string {
	p.t.Helper()
	select {
	case line := <-p.lines:
		return line
	case <-time.After(2 * time.Second):
		p.t.Fatal("timed out waiting for output")
		return ""
	}
}

func (p *peer) expect(want string) {
	p.t.Helper()
	assertJSON(p.t, p.recv(), want)
}

func (p *peer) expectSilence() {
	p.t.Helper()
	select {
	case line := <-p.lines:
		p.t.Fatalf("unexpected output: %s", line)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func echo(_ context.Context, params json.RawMessage) (any, error) { return params, nil }

func TestErrorsForMalformedInput(t *testing.T) {
	p := newPeer(t, Methods{"echo": echo})
	tests := []struct {
		name, line, want string
	}{
		{"parse error", `{"jsonrpc":`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`},
		{"not an object", `42`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: json: cannot unmarshal number into Go value of type jsonrpc.inbound"}}`},
		{"bad id", `{"jsonrpc":"2.0","id":{},"method":"echo"}`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: id must be a string, a number or null"}}`},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"echo"}`, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"invalid request: want jsonrpc \"2.0\" and a method"}}`},
		{"unknown method", `{"jsonrpc":"2.0","id":2,"method":"nope"}`, `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found: nope"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p.t = t
			p.send(tt.line)
			p.expect(tt.want)
		})
	}
}

func TestEchoesIDsVerbatim(t *testing.T) {
	p := newPeer(t, Methods{"echo": echo})
	for _, id := range []string{`7`, `"req-7"`, `null`, `12345678901234567890`} {
		p.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"echo","params":{"x":1}}`)
		p.expect(`{"jsonrpc":"2.0","id":` + id + `,"result":{"x":1}}`)
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	got := make(chan json.RawMessage, 1)
	p := newPeer(t, Methods{
		"note": func(_ context.Context, params json.RawMessage) (any, error) {
			got <- params
			return nil, errors.New("ignored")
		},
	})
	p.send(`{"jsonrpc":"2.0","method":"note","params":{"a":true}}`)
	p.send(`{"jsonrpc":"2.0","method":"unknown"}`)
	p.expectSilence()
	assertJSON(t, string(<-got), `{"a":true}`)
}

func TestDeferredReplyIsWrittenBeforeLaterNotifications(t *testing.T) {
	var conn *Conn
	p := newPeer(t, Methods{
		"later": func(context.Context, json.RawMessage) (any, error) {
			reply := NewReply()
			go func() {
				reply.Resolve(map[string]string{"messageId": "m1"})
				_ = conn.Notify("session/update", map[string]string{"after": "reply"})
			}()
			return reply, nil
		},
	})
	conn = p.conn
	p.send(`{"jsonrpc":"2.0","id":1,"method":"later"}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"result":{"messageId":"m1"}}`)
	p.expect(`{"jsonrpc":"2.0","method":"session/update","params":{"after":"reply"}}`)
}

func TestReplySettledBeforeHandlerReturns(t *testing.T) {
	p := newPeer(t, Methods{
		"now": func(context.Context, json.RawMessage) (any, error) {
			reply := NewReply()
			reply.Reject(NewError(-32002, "gone"))
			reply.Resolve("ignored")
			return reply, nil
		},
	})
	p.send(`{"jsonrpc":"2.0","id":"a","method":"now"}`)
	p.expect(`{"jsonrpc":"2.0","id":"a","error":{"code":-32002,"message":"gone"}}`)
}

func TestCancelRequestCancelsDeferredRequestContext(t *testing.T) {
	p := newPeer(t, Methods{
		"wait": func(ctx context.Context, _ json.RawMessage) (any, error) {
			reply := NewReply()
			context.AfterFunc(ctx, func() { reply.Reject(NewError(-32800, "Request cancelled")) })
			return reply, nil
		},
	})
	p.send(`{"jsonrpc":"2.0","id":"w","method":"wait"}`)
	p.expectSilence()
	p.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"w"}}`)
	p.expect(`{"jsonrpc":"2.0","id":"w","error":{"code":-32800,"message":"Request cancelled"}}`)
}

func TestHandlerErrors(t *testing.T) {
	p := newPeer(t, Methods{
		"plain": func(context.Context, json.RawMessage) (any, error) { return nil, errors.New("boom") },
		"panic": func(context.Context, json.RawMessage) (any, error) { panic("oops") },
		"typed": Handle(func(_ context.Context, p validated) (any, error) { return p, nil }),
	})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"plain"}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"boom"}}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"panic"}`)
	p.expect(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"internal error"}}`)
	p.send(`{"jsonrpc":"2.0","id":3,"method":"typed","params":{"name":""}}`)
	p.expect(`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"invalid params: name is required"}}`)
	p.send(`{"jsonrpc":"2.0","id":4,"method":"typed","params":{"name":7}}`)
	p.expect(`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"invalid params: json: cannot unmarshal number into Go struct field validated.name of type string"}}`)
	p.send(`{"jsonrpc":"2.0","id":5,"method":"typed","params":{"name":"ok","extra":true}}`)
	p.expect(`{"jsonrpc":"2.0","id":5,"result":{"name":"ok"}}`)
}

type validated struct {
	Name string `json:"name"`
}

func (v validated) Validate() error {
	if v.Name == "" {
		return errors.New("name is required")
	}
	return nil
}

func TestBatch(t *testing.T) {
	release := make(chan struct{})
	p := newPeer(t, Methods{
		"echo": echo,
		"slow": func(context.Context, json.RawMessage) (any, error) {
			reply := NewReply()
			go func() { <-release; reply.Resolve("slow") }()
			return reply, nil
		},
	})

	t.Run("mixed", func(t *testing.T) {
		p.t = t
		p.send(`[{"jsonrpc":"2.0","id":1,"method":"slow"},{"jsonrpc":"2.0","method":"echo"},1,{"jsonrpc":"2.0","id":2,"method":"echo","params":[2]}]`)
		p.expectSilence()
		close(release)
		var got []map[string]any
		if err := json.Unmarshal([]byte(p.recv()), &got); err != nil {
			t.Fatal(err)
		}
		byID := map[any]map[string]any{}
		for _, r := range got {
			byID[r["id"]] = r
		}
		if len(got) != 3 || byID[1.0]["result"] != "slow" || byID[nil]["error"] == nil {
			t.Fatalf("unexpected batch response: %v", got)
		}
		if !reflect.DeepEqual(byID[2.0]["result"], []any{2.0}) {
			t.Fatalf("unexpected echo result: %v", byID[2.0])
		}
	})
	t.Run("empty", func(t *testing.T) {
		p.t = t
		p.send(`[]`)
		p.expect(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid batch"}}`)
	})
	t.Run("notifications only", func(t *testing.T) {
		p.t = t
		p.send(`[{"jsonrpc":"2.0","method":"echo"},{"jsonrpc":"2.0","method":"echo"}]`)
		p.expectSilence()
	})
}

func TestMarshalKeepsHTMLCharacters(t *testing.T) {
	b, err := Marshal(map[string]string{"code": "a < b && c > d"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"code":"a < b && c > d"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	conn := NewConn(nil, io.Discard, slog.New(slog.DiscardHandler))
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Notify("x", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}
