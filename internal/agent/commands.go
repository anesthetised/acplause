package agent

import (
	"context"
	"strings"
	"unicode"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

const compactName = "compact"

func availableCommands() []acp.AvailableCommand {
	return []acp.AvailableCommand{{
		Type:        "command",
		Name:        compactName,
		Description: "Summarize older conversation history to free model context.",
	}}
}

// classifyCompact checks the first original text block so neither flattened
// resource text nor a later text block can invoke the command.
func classifyCompact(prompt []acp.ContentBlock) (bool, error) {
	for _, block := range prompt {
		if block.Type != acp.ContentText {
			continue
		}
		rest, found := strings.CutPrefix(block.Text, "/"+compactName)
		if !found {
			return false, nil
		}
		args := strings.TrimLeftFunc(rest, unicode.IsSpace)
		if rest != "" && args == rest {
			return false, nil // The command name continues past /compact.
		}
		if args != "" || len(prompt) != 1 {
			return false, acp.ErrInvalidParams("Send /compact by itself, then send text or attachments separately.")
		}
		return true, nil
	}
	return false, nil
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
