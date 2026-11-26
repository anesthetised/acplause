package jsonrpc

// Regression tests for #54: deferred replies, settlement and ordering.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestIssue54EarlyReplyPrecedesNotification(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "resolve"
		want := `{"jsonrpc":"2.0","id":1,"result":{"messageId":"m1"}}`
		if reject {
			name = "reject"
			want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32800,"message":"cancelled"}}`
		}
		t.Run(name, func(t *testing.T) {
			var conn *Conn
			p := newPeer(t, map[string]Handler{
				"early": func(ctx context.Context, _ json.RawMessage) (any, error) {
					reply := NewReply(ctx)
					notified := make(chan error, 1)
					go func() {
						if reject {
							reply.Reject(NewError(-32800, "cancelled"))
						} else {
							reply.Resolve(map[string]string{"messageId": "m1"})
						}
						notified <- conn.Notify("session/update", map[string]string{"after": "reply"})
					}()
					// This barrier forces the legal early-worker schedule. It is
					// a reproduction aid, not a production handler pattern.
					select {
					case err := <-notified:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(2 * time.Second):
						t.Error("worker could not settle and notify before handler return")
					}
					return reply, nil
				},
			})
			conn = p.conn
			p.send(`{"jsonrpc":"2.0","id":1,"method":"early"}`)
			p.expect(want)
			p.expect(`{"jsonrpc":"2.0","method":"session/update","params":{"after":"reply"}}`)
		})
	}
}

func TestIssue54SettlementWinsOverHandlerCompletion(t *testing.T) {
	for _, reject := range []bool{false, true} {
		for _, completion := range []string{"return", "error", "panic"} {
			name := "resolve/"
			want := `{"jsonrpc":"2.0","id":1,"result":"first"}`
			if reject {
				name = "reject/"
				want = `{"jsonrpc":"2.0","id":1,"error":{"code":-32800,"message":"first"}}`
			}
			t.Run(name+completion, func(t *testing.T) {
				p := newPeer(t, map[string]Handler{
					"first": func(ctx context.Context, _ json.RawMessage) (any, error) {
						reply := NewReply(ctx)
						if reject {
							reply.Reject(NewError(-32800, "first"))
						} else {
							reply.Resolve("first")
						}
						switch completion {
						case "error":
							return nil, errors.New("later error")
						case "panic":
							panic("later panic")
						default:
							return "later return", nil
						}
					},
					"echo": echo,
				})
				p.send(`{"jsonrpc":"2.0","id":1,"method":"first"}`)
				p.expect(want)
				p.send(`{"jsonrpc":"2.0","id":2,"method":"echo","params":["next"]}`)
				p.expect(`{"jsonrpc":"2.0","id":2,"result":["next"]}`)
				p.conn.mu.Lock()
				defer p.conn.mu.Unlock()
				if len(p.conn.inflight) != 0 {
					t.Fatal("completed request retained for cancellation")
				}
			})
		}
	}
}

func TestIssue54HandlerCompletionWinsOverLateSettlement(t *testing.T) {
	for _, completion := range []string{"return", "error", "panic"} {
		t.Run(completion, func(t *testing.T) {
			replies := make(chan *Reply, 1)
			p := newPeer(t, map[string]Handler{
				"first": func(ctx context.Context, _ json.RawMessage) (any, error) {
					replies <- NewReply(ctx)
					switch completion {
					case "error":
						return nil, NewError(-32002, "first")
					case "panic":
						panic("first")
					default:
						return "first", nil
					}
				},
				"echo": echo,
			})
			p.send(`{"jsonrpc":"2.0","id":1,"method":"first"}`)
			switch completion {
			case "error":
				p.expect(`{"jsonrpc":"2.0","id":1,"error":{"code":-32002,"message":"first"}}`)
			case "panic":
				p.expect(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`)
			default:
				p.expect(`{"jsonrpc":"2.0","id":1,"result":"first"}`)
			}
			reply := <-replies
			reply.Resolve("ignored")
			reply.Reject(NewError(-32800, "ignored"))
			p.send(`{"jsonrpc":"2.0","id":2,"method":"echo","params":["next"]}`)
			p.expect(`{"jsonrpc":"2.0","id":2,"result":["next"]}`)
		})
	}
}

func TestIssue54SynchronousResolveKeepsFirstSettlement(t *testing.T) {
	p := newPeer(t, map[string]Handler{
		"now": func(ctx context.Context, _ json.RawMessage) (any, error) {
			derived, cancel := context.WithCancel(ctx)
			defer cancel()
			reply := NewReply(derived)
			reply.Resolve("first")
			reply.Reject(NewError(-32800, "ignored"))
			if !errors.Is(derived.Err(), context.Canceled) {
				t.Error("settlement did not cancel the request's derived context")
			}
			return reply, nil
		},
		"echo": echo,
	})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"now"}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"result":"first"}`)
	// A second request fences delivery, including any duplicate settlement.
	p.send(`{"jsonrpc":"2.0","id":2,"method":"echo","params":["next"]}`)
	p.expect(`{"jsonrpc":"2.0","id":2,"result":["next"]}`)
}

func TestIssue54StandaloneReplyStoresSettlement(t *testing.T) {
	reply := NewReply(context.Background())
	reply.Resolve("stored")
	p := newPeer(t, map[string]Handler{
		"stored": func(context.Context, json.RawMessage) (any, error) { return reply, nil },
	})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"stored"}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"result":"stored"}`)
}

func TestIssue54NotificationReplyProducesNoResponse(t *testing.T) {
	p := newPeerLimit(t, Methods{
		Requests: map[string]Handler{"echo": echo},
		Notifications: map[string]Handler{
			"notice": func(ctx context.Context, _ json.RawMessage) (any, error) {
				reply := NewReply(ctx)
				reply.Resolve("ignored")
				return reply, nil
			},
		},
	}, maxLine)
	p.send(`{"jsonrpc":"2.0","method":"notice"}`)
	p.send(`{"jsonrpc":"2.0","id":1,"method":"echo","params":["next"]}`)
	p.expect(`{"jsonrpc":"2.0","id":1,"result":["next"]}`)
}

func TestIssue54PendingReplyAllowsUnrelatedOutput(t *testing.T) {
	var conn *Conn
	var reply *Reply
	p := newPeer(t, map[string]Handler{
		"wait": func(ctx context.Context, _ json.RawMessage) (any, error) {
			reply = NewReply(ctx)
			notified := make(chan error, 1)
			go func() {
				notified <- conn.Notify("session/update", map[string]string{"sessionId": "other"})
			}()
			// An admission lock must not hold another session's output while
			// this handler is running, even before its Reply is returned.
			select {
			case err := <-notified:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(2 * time.Second):
				t.Error("handler admission blocked an unrelated notification")
			}
			return reply, nil
		},
		"echo": echo,
	})
	conn = p.conn
	p.send(`{"jsonrpc":"2.0","id":1,"method":"wait"}`)
	p.expect(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"other"}}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"echo","params":["other session"]}`)
	p.expect(`{"jsonrpc":"2.0","id":2,"result":["other session"]}`)
	reply.Resolve("finished")
	p.expect(`{"jsonrpc":"2.0","id":1,"result":"finished"}`)
}

func TestIssue54ServeExitCancelsPendingReply(t *testing.T) {
	p := newPeer(t, map[string]Handler{
		"wait": func(ctx context.Context, _ json.RawMessage) (any, error) {
			reply := NewReply(ctx)
			context.AfterFunc(ctx, func() { reply.Reject(NewError(-32800, "cancelled")) })
			return reply, nil
		},
		"echo": echo,
	})
	p.send(`{"jsonrpc":"2.0","id":1,"method":"wait"}`)
	p.send(`{"jsonrpc":"2.0","id":2,"method":"echo","params":["admitted"]}`)
	p.expect(`{"jsonrpc":"2.0","id":2,"result":["admitted"]}`)
	if err := p.in.Close(); err != nil {
		t.Fatal(err)
	}
	p.expect(`{"jsonrpc":"2.0","id":1,"error":{"code":-32800,"message":"cancelled"}}`)
	if err := p.conn.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIssue54ReplyPreservesBackpressureAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, out, release := heldConn(t)
		var reply *Reply
		conn.handleLine(context.Background(), Methods{Requests: map[string]Handler{
			"wait": func(ctx context.Context, _ json.RawMessage) (any, error) {
				reply = NewReply(ctx)
				return reply, nil
			},
		}}, []byte(`{"jsonrpc":"2.0","id":1,"method":"wait"}`))
		_ = conn.Notify("block_writer", nil)
		synctest.Wait()
		for range cap(conn.ordinary.lines) {
			_ = conn.Notify("filler", nil)
		}
		settled := make(chan struct{})
		go func() { reply.Resolve("done"); close(settled) }()
		synctest.Wait()
		select {
		case <-settled:
			t.Fatal("Resolve returned before the response could be queued")
		default:
		}
		closed := make(chan struct{})
		go func() { _ = conn.Close(); close(closed) }()
		synctest.Wait()
		select {
		case <-settled:
		default:
			t.Fatal("Close did not release the blocked response enqueue")
		}
		reply.Reject(NewError(-32800, "ignored"))
		if err := conn.Notify("closed", nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("Notify after Close = %v; want ErrClosed", err)
		}
		release()
		<-closed
		if strings.Contains(out.String(), `"id":1`) {
			t.Fatalf("response was written after its enqueue was closed: %s", out.String())
		}
	})
}
