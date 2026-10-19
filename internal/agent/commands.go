package agent

import (
	"context"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

func availableCommands() []acp.AvailableCommand {
	return []acp.AvailableCommand{{Name: "compact", Description: "Summarize older conversation history to free model context."}}
}

// isCompactCommand checks the original blocks, never flattened resource text.
func isCompactCommand(prompt []acp.ContentBlock) bool {
	return len(prompt) == 1 && prompt[0].Type == acp.ContentText && strings.TrimSpace(prompt[0].Text) == "/compact"
}

// compactCommand runs after insertion on the worker, sharing automatic
// compaction's validation, accounting and atomic history/replay replacement.
func (s *Session) compactCommand(ctx context.Context) (acp.StopReason, *jsonrpc.Error) {
	var remaining *int
	if s.deps.maxRounds > 0 {
		remaining = new(s.deps.maxRounds)
	}
	round, compacted, err := s.prepareModelRequest(ctx, remaining, true)
	if err != nil {
		return acp.StopError, turnError(err)
	}
	if err := ctx.Err(); err != nil {
		return acp.StopCancelled, nil
	}
	text := "Conversation context compacted."
	if !compacted {
		text = "Nothing to compact: there is no older conversation history."
		if s.catalog.contextLength(round.Model) == 0 {
			text = "Compaction was not performed: the selected model has no known context window."
		}
	}
	out := roundOutput{s: s}
	out.delta(llm.Delta{Kind: llm.DeltaText, Text: text})
	s.appendHistory(llm.Message{Role: llm.RoleAssistant, Content: text})
	return acp.StopEndTurn, nil
}
