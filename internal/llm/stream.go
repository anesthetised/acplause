package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
)

// chunk is one streamed chat.completion.chunk.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content          string            `json:"content"`
			Reasoning        string            `json:"reasoning"`
			ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
			ToolCalls        []toolCallDelta   `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage    `json:"usage"`
	Error *apiError `json:"error"`
}

// toolCallDelta is a fragment of a tool call. The id and name arrive in the
// first fragment for an index; arguments are split across fragments.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// readStream consumes an SSE body until [DONE], reporting text and reasoning
// through onDelta as they arrive.
func readStream(ctx context.Context, body io.Reader, onDelta func(Delta)) (*Completion, error) {
	events := newSSEReader(body)
	var acc accumulator
	for {
		data, err := events.Next()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case errors.Is(err, io.EOF):
				return nil, &Error{Kind: KindTruncated, Message: "stream ended before [DONE]"}
			}
			return nil, &Error{Kind: KindUnreachable, Message: err.Error()}
		}
		if string(data) == "[DONE]" {
			return acc.completion(), nil
		}

		var c chunk
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, &Error{Kind: KindProtocol, Message: "malformed chunk: " + err.Error()}
		}
		if c.Error != nil {
			return nil, c.Error.toError()
		}
		acc.add(&c, onDelta)
	}
}

// accumulator assembles a Completion from chunks.
type accumulator struct {
	content   strings.Builder
	reasoning strings.Builder
	details   []ReasoningDetail
	calls     map[int]*ToolCall
	finish    string
	usage     *Usage
}

func (a *accumulator) add(c *chunk, onDelta func(Delta)) {
	if c.Usage != nil {
		a.usage = c.Usage
	}
	if len(c.Choices) == 0 {
		return
	}
	choice := c.Choices[0]
	if choice.FinishReason != "" {
		a.finish = choice.FinishReason
	}

	delta := choice.Delta
	reasoning := delta.Reasoning
	if reasoning == "" {
		reasoning = detailsText(delta.ReasoningDetails)
	}
	if reasoning != "" {
		a.reasoning.WriteString(reasoning)
		onDelta(Delta{Kind: DeltaReasoning, Text: reasoning})
	}
	for _, d := range delta.ReasoningDetails {
		a.details = mergeDetail(a.details, d)
	}
	if delta.Content != "" {
		a.content.WriteString(delta.Content)
		onDelta(Delta{Kind: DeltaText, Text: delta.Content})
	}
	for _, f := range delta.ToolCalls {
		a.addToolCall(f)
	}
}

func (a *accumulator) addToolCall(f toolCallDelta) {
	if a.calls == nil {
		a.calls = make(map[int]*ToolCall)
	}
	call, ok := a.calls[f.Index]
	if !ok {
		call = &ToolCall{Type: "function"}
		a.calls[f.Index] = call
	}
	if call.ID == "" {
		call.ID = f.ID
	}
	if call.Function.Name == "" {
		call.Function.Name = f.Function.Name
	}
	call.Function.Arguments += f.Function.Arguments
}

func (a *accumulator) completion() *Completion {
	c := &Completion{
		Content:          a.content.String(),
		Reasoning:        a.reasoning.String(),
		ReasoningDetails: a.details,
		FinishReason:     a.finish,
		Usage:            a.usage,
	}
	for _, index := range slices.Sorted(maps.Keys(a.calls)) {
		call := *a.calls[index]
		if call.ID == "" {
			call.ID = "call_" + rand.Text()
		}
		c.ToolCalls = append(c.ToolCalls, call)
	}
	return c
}

// Streamed reasoning details are fragments of blocks identified by index and
// type: their payload strings concatenate, other fields take the latest
// value.
var detailPayloads = []string{"text", "summary", "data"}

func mergeDetail(details []ReasoningDetail, fragment ReasoningDetail) []ReasoningDetail {
	for _, d := range slices.Backward(details) {
		if d["index"] == fragment["index"] && d["type"] == fragment["type"] {
			for key, value := range fragment {
				if prev, ok := d[key].(string); ok && slices.Contains(detailPayloads, key) {
					next, _ := value.(string)
					d[key] = prev + next
				} else if value != nil {
					d[key] = value
				}
			}
			return details
		}
	}
	return append(details, maps.Clone(fragment))
}

// detailsText extracts readable reasoning from details, for models that
// stream reasoning only in structured form.
func detailsText(details []ReasoningDetail) string {
	var b strings.Builder
	for _, d := range details {
		for _, key := range []string{"text", "summary"} {
			if s, ok := d[key].(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}
