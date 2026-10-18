package agent

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const systemTemplate = `You are acplause, a coding agent working in the user's workspace through tools.
Read and search before you change code, prefer small edits, and run commands to check your work.
Paths are relative to the working directory unless absolute. Keep answers concise.

Working directory: %s`

func systemPrompt(cwd string, dirs []string) string {
	prompt := fmt.Sprintf(systemTemplate, cwd)
	if len(dirs) > 0 {
		prompt += "\nAdditional directories: " + strings.Join(dirs, ", ")
	}
	return prompt
}

// promptText renders a prompt for the model. Content beyond the baseline
// text and resource links is not advertised and is skipped.
func promptText(blocks []acp.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case acp.ContentText:
			parts = append(parts, b.Text)
		case acp.ContentResourceLink:
			parts = append(parts, fmt.Sprintf("[%s](%s)", b.Name, b.URI))
		}
	}
	return cmp.Or(strings.Join(parts, "\n\n"), "(empty message)")
}

// title names a session after the first line of its first prompt.
func title(blocks []acp.ContentBlock) string {
	const width = 80
	line, _, _ := strings.Cut(strings.TrimSpace(promptText(blocks)), "\n")
	if runes := []rune(line); len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return line
}

// rawInput is the tool input as JSON: the arguments themselves when they
// parse, otherwise the raw text as a string.
func rawInput(args string) json.RawMessage {
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return jsonString(args)
}

func jsonString(s string) json.RawMessage {
	b, _ := jsonrpc.Marshal(s)
	return b
}
