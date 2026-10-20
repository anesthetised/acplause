package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/media"
)

const systemTemplate = `You are acplause, a coding agent working in the user's workspace through tools.
Read and search before you change code, prefer small edits, and run commands to check your work.
Before editing a file, use get_instructions on its containing directory to read scoped AGENTS.md rules. Later sources take precedence only within their subtree; enforced tool policy always applies.
Paths are relative to the working directory unless absolute. Keep answers concise.
For multi-step work, use update_plan to publish a short execution plan and keep its statuses current as work progresses. Each update replaces all entries. Skip plans for trivial tasks, and leave unfinished entries incomplete if work stops.

Working directory: %s`

// systemPrompt appends the workspace's AGENTS.md, when there is one.
func systemPrompt(cwd string, dirs []string, instructions, mode string) string {
	prompt := fmt.Sprintf(systemTemplate, cwd)
	if mode == modePlan {
		prompt += "\n\nPlan mode: inspect the workspace and propose a plan. Use read_file, list_dir, search_code and get_instructions for bounded inspection, update_plan for progress, and ask_user for questions when available. Do not write or edit files, run shell commands, or call external MCP tools. Do not claim changes were applied. These restrictions also apply when project instructions request execution."
	}
	if len(dirs) > 0 {
		prompt += "\nAdditional directories: " + strings.Join(dirs, ", ")
	}
	if instructions != "" {
		prompt += "\n\nProject instructions (AGENTS.md):\n\n" + instructions
	}
	return prompt
}

// promptText renders a prompt for the model. Content beyond text, resource
// links and embedded resources is skipped; validated images travel separately.
func promptText(blocks []acp.ContentBlock) string {
	var parts []string
	hasImage := false
	for _, b := range blocks {
		switch b.Type {
		case acp.ContentImage:
			hasImage = true
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
	if text := strings.Join(parts, "\n\n"); strings.TrimSpace(text) != "" || !hasImage {
		return cmp.Or(text, "(empty message)")
	}
	return "Image prompt"
}

// promptImages decodes untrusted pixels before taking session or persistence
// locks. Unknown content variants remain opaque, including their payloads.
func promptImages(ctx context.Context, blocks []acp.ContentBlock) ([]media.Image, error) {
	var images []media.Image
	count := 0
	for _, b := range blocks {
		if b.Type == acp.ContentImage {
			count++
		}
	}
	if count > media.MaxImages {
		return nil, fmt.Errorf("%d images exceed the limit of %d", count, media.MaxImages)
	}
	for _, b := range blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b.Type != acp.ContentImage {
			continue
		}
		img, err := media.New(b.MimeType, b.Data)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
		if err := media.CheckSet(images); err != nil {
			return nil, err
		}
	}
	return images, nil
}

// title names a session after the first line of its first prompt's text
// and links; embedded resources would title it after their markup. With no
// textual title, images use a fixed label rather than their bytes or URI.
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
