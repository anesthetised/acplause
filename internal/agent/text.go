package agent

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const systemTemplate = `You are acplause, a coding agent working in the user's workspace through tools.
Read and search before you change code, prefer small edits, and run commands to check your work.
Paths are relative to the working directory unless absolute. Keep answers concise.

Working directory: %s`

// systemPrompt appends the workspace's AGENTS.md, when there is one.
func systemPrompt(cwd string, dirs []string, instructions string) string {
	prompt := fmt.Sprintf(systemTemplate, cwd)
	if len(dirs) > 0 {
		prompt += "\nAdditional directories: " + strings.Join(dirs, ", ")
	}
	if instructions != "" {
		prompt += "\n\nProject instructions (AGENTS.md):\n\n" + instructions
	}
	return prompt
}

// promptText renders a prompt for the model. Content beyond text, resource
// links and embedded resources is not advertised and is skipped.
func promptText(blocks []acp.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case acp.ContentText:
			parts = append(parts, b.Text)
		case acp.ContentResourceLink:
			parts = append(parts, fmt.Sprintf("[%s](%s)", b.Name, b.URI))
		case acp.ContentResource:
			if r := b.Resource; r != nil && r.Text != nil {
				parts = append(parts, fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", r.URI, *r.Text))
			} else if r != nil {
				parts = append(parts, fmt.Sprintf("[binary resource omitted: %s]", r.URI))
			}
		}
	}
	return cmp.Or(strings.Join(parts, "\n\n"), "(empty message)")
}

// title names a session after the first line of its first prompt's text
// and links; embedded resources would title it after their markup.
func title(blocks []acp.ContentBlock) string {
	const width = 80
	blocks = slices.DeleteFunc(slices.Clone(blocks), func(b acp.ContentBlock) bool { return b.Type == acp.ContentResource })
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
