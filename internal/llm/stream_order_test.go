package llm

import (
	"context"
	"strings"
	"testing"
)

func TestChatPreservesToolCallIndexOrder(t *testing.T) {
	stream := data(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"edit","function":{"name":"edit","arguments":"{}"}}]}}]}`) +
		data(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"create","function":{"name":"write_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`) + data(`[DONE]`)
	c, err := readStream(context.Background(), strings.NewReader(stream), func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.ToolCalls) != 2 || c.ToolCalls[0].ID != "create" || c.ToolCalls[1].ID != "edit" {
		t.Fatalf("explicit index order lost: %+v", c.ToolCalls)
	}
}
