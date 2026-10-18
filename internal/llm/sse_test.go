package llm

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSSEReader(t *testing.T) {
	stream := ": OPENROUTER PROCESSING\r\n\r\n" +
		"event: message\ndata: {\"a\":1}\n\n" +
		"data:first\ndata: second\nid: 7\n\n" +
		"\n\n" +
		"data: [DONE]"
	r := newSSEReader(strings.NewReader(stream))
	for _, want := range []string{`{"a":1}`, "first\nsecond", "[DONE]"} {
		got, err := r.Next()
		if err != nil || string(got) != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}
}
