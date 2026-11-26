package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
)

func TestBeforeWriteRunsOnlyForEncodableSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result any
		err    error
		want   int32
	}{
		{name: "success", result: "ready", want: 1},
		{name: "handler error", err: errors.New("failed")},
		{name: "encoding error", result: make(chan int)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := newPeer(t, Methods{"resume": func(context.Context, json.RawMessage) (any, error) {
				return OnWrite(tc.result, func() { calls.Add(1) }, nil), tc.err
			}})
			p.send(`{"jsonrpc":"2.0","id":1,"method":"resume"}`)
			var r response
			if err := json.Unmarshal([]byte(p.recv()), &r); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != tc.want || (r.Error == nil) != (tc.want == 1) {
				t.Fatalf("before-write calls %d, response %+v", calls.Load(), r)
			}
		})
	}
}

func TestCloseDuringResponseWriteInvalidatesDelivery(t *testing.T) {
	c, _, release := heldConn(t)
	entered := make(chan struct{})
	outcome := make(chan error, 1)
	r := newResponse(ID("1"), OnWrite("ready", func() { close(entered) }, func(err error) { outcome <- err }), nil)
	if err := c.send(r); err != nil {
		t.Fatal(err)
	}
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	<-c.quit
	release()
	if err := <-outcome; !errors.Is(err, ErrClosed) {
		t.Fatalf("write outcome = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("successful flush = %v", err)
	}
}
