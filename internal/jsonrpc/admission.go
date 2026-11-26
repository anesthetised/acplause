package jsonrpc

import (
	"context"
	"errors"
)

// Admission orders an ordinary enqueue against an application barrier. The
// caller has acquired capacity. The action may call commit once, synchronously,
// or return without consuming the slot. Neither path waits for the writer.
type Admission func(commit func() error) error

type admissionKey struct{}

// WithAdmission applies a final enqueue check to a worker's outgoing request.
func WithAdmission(ctx context.Context, admit Admission) context.Context {
	return context.WithValue(ctx, admissionKey{}, admit)
}

// NotificationLine encodes a complete notification without its NDJSON newline.
func NotificationLine(method string, params any) ([]byte, error) {
	return Marshal(notification{JSONRPC: Version, Method: method, Params: params})
}

// ErrAdmissionChanged asks a waiting sender to check its application barrier again.
var ErrAdmissionChanged = errors.New("jsonrpc: admission changed")

// SendLine queues an encoded message, checking admission after any capacity wait.
// Closing changed interrupts that wait so an application barrier can take the line.
func (c *Conn) SendLine(ctx context.Context, line []byte, changed <-chan struct{}, admit Admission) error {
	return c.sendFrame(ctx, outputFrame{line: line}, "", pendingCall{}, changed, admit)
}

// SendLines queues consecutive NDJSON messages in one ordinary slot.
func (c *Conn) SendLines(ctx context.Context, lines [][]byte, admit Admission) error {
	return c.sendFrame(ctx, outputFrame{lines: lines}, "", pendingCall{}, nil, admit)
}

// Reservation owns one ordinary slot until Enqueue transfers it to the writer.
type Reservation struct{ c *Conn }

// TryReserve returns nil when ordinary capacity is unavailable at receipt.
func (c *Conn) TryReserve() *Reservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.quit:
		return nil
	default:
	}
	select {
	case c.ordinary.slots <- struct{}{}:
		return &Reservation{c}
	default:
		return nil
	}
}

// Enqueue transfers the reservation exactly once. Lines are separate NDJSON
// messages in one ordered item. On success the writer calls taken on dequeue.
func (r *Reservation) Enqueue(prefix func() ([]byte, error), lines [][]byte, taken func()) error {
	c := r.c
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.quit:
		<-c.ordinary.slots
		return ErrClosed
	default:
	}
	c.enqueueLocked(&c.ordinary, outputFrame{prefix: prefix, lines: lines, taken: taken})
	return nil
}
