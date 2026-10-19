package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

var errStreamInterrupted = errors.New("MCP event stream interrupted")
var errEventLimit = errors.New("MCP event exceeds 8 MiB")

type event struct {
	data     []byte
	id       string
	hasID    bool
	retry    time.Duration
	hasRetry bool
}

// eventReader retains MCP replay cursors and retry instructions. The LLM SSE
// reader intentionally ignores these fields and cannot serve this transport.
type eventReader struct{ scanner *bufio.Scanner }

func newEventReader(r io.Reader) *eventReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), maxMessage+1)
	return &eventReader{scanner: sc}
}
func (r *eventReader) next() (event, error) {
	var e event
	var data bytes.Buffer
	size := 0
	for r.scanner.Scan() {
		line := r.scanner.Text()
		size += len(line)
		if size > maxMessage {
			return e, errEventLimit
		}
		if line == "" {
			if data.Len() > 0 || e.hasID || e.hasRetry {
				e.data = bytes.TrimSuffix(data.Bytes(), []byte("\n"))
				return e, nil
			}
			size = 0
			continue
		}
		if line[0] == ':' {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		case "id":
			if !strings.ContainsRune(value, 0) {
				e.id = value
				e.hasID = true
			}
		case "retry":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 && n <= int64((1<<63-1)/time.Millisecond) {
				e.retry = time.Duration(n) * time.Millisecond
				e.hasRetry = true
			}
		}
	}
	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return e, errEventLimit
		}
		return e, errStreamInterrupted
	}
	// SSE dispatch requires an empty line; an unterminated event is not complete.
	return e, io.EOF
}
