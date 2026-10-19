package mcp

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMCPSSEFields(t *testing.T) {
	r := newEventReader(strings.NewReader(": ping\r\nid: cursor\r\nretry: 17\r\ndata: {\r\ndata: \"a\":1}\r\n\r\nid:\ndata:\n\n"))
	e, err := r.next()
	if err != nil || e.id != "cursor" || !e.hasID || e.retry != 17*time.Millisecond || string(e.data) != "{\n\"a\":1}" {
		t.Fatalf("event=%+v err=%v", e, err)
	}
	e, err = r.next()
	if err != nil || !e.hasID || e.id != "" || len(e.data) != 0 {
		t.Fatalf("empty id: %+v %v", e, err)
	}
	if _, err = r.next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	r = newEventReader(strings.NewReader("data: " + strings.Repeat("x", maxMessage) + "\n\n"))
	if _, err = r.next(); !errors.Is(err, errEventLimit) {
		t.Fatalf("limit: %v", err)
	}
}
