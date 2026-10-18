package llm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// sseReader yields the data of each server-sent event. Comment lines (such
// as OpenRouter's ": OPENROUTER PROCESSING" keepalive) and fields other than
// data are skipped.
type sseReader struct {
	r    *bufio.Reader
	data bytes.Buffer
}

func newSSEReader(r io.Reader) *sseReader { return &sseReader{r: bufio.NewReader(r)} }

// Next returns the next event's data, or io.EOF when the stream ends.
func (s *sseReader) Next() ([]byte, error) {
	for {
		line, err := s.r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		atEOF := err != nil
		line = bytes.TrimRight(line, "\r\n")

		switch {
		case len(line) == 0:
			if s.data.Len() > 0 {
				return s.take(), nil
			}
		case line[0] == ':':
		default:
			field, value, _ := bytes.Cut(line, []byte(":"))
			if string(field) == "data" {
				if s.data.Len() > 0 {
					s.data.WriteByte('\n')
				}
				s.data.Write(bytes.TrimPrefix(value, []byte(" ")))
			}
		}

		if atEOF {
			if s.data.Len() > 0 {
				return s.take(), nil
			}
			return nil, io.EOF
		}
	}
}

func (s *sseReader) take() []byte {
	data := bytes.Clone(s.data.Bytes())
	s.data.Reset()
	return data
}
