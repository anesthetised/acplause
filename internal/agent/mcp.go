package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
	"github.com/anesthetised/acplause/internal/mcp"
	"github.com/anesthetised/acplause/internal/media"
	"github.com/anesthetised/acplause/internal/tools"
)

// modelRound keeps the routing decisions that produced the advertised tools.
// A later resume cannot redirect a response from this request to a new server.
type modelRound struct {
	llm.Request
	mode     string // execution policy at the time tools were advertised
	bindings map[string]mcp.Tool
}

type preparedTool struct {
	observeShell   func() *tools.ShellObserver
	Title, Command string
	Locations      []string
	permission     bool
	binding        context.Context
	preview        func(context.Context) (*acp.ToolCallContent, error)
	run            func(context.Context) toolOutput
}

func (c *preparedTool) Preview(ctx context.Context) (*acp.ToolCallContent, error) {
	if c.preview != nil {
		return c.preview(ctx)
	}
	return nil, nil
}

type toolOutput struct {
	Output    string
	Failed    bool
	Content   []acp.ToolCallContent
	RawOutput json.RawMessage
	Images    []media.Image
}

func builtinOutput(r tools.Result) toolOutput {
	out := toolOutput{Output: r.Output, Failed: r.Failed, Content: []acp.ToolCallContent{acp.ToolText(r.Output)}, RawOutput: jsonString(r.Output)}
	if r.Diff != nil && !r.Failed {
		out.Content = append([]acp.ToolCallContent{*r.Diff}, out.Content...)
	}
	return out
}
func prepareBuiltin(tb *tools.Toolbox, name, args string) (*preparedTool, error) {
	c, err := tb.Prepare(name, args)
	if err != nil {
		return nil, err
	}
	prepared := &preparedTool{Title: c.Title, Command: c.Command, Locations: c.Locations, permission: asksPermission(tb.Kind(name)), preview: c.Preview, run: func(ctx context.Context) toolOutput { return builtinOutput(c.Run(ctx)) }}
	if c.Command != "" {
		prepared.observeShell = c.ObserveShell
	}
	return prepared, nil
}
func prepareMCP(t mcp.Tool, args string, vision bool) (*preparedTool, error) {
	raw, err := mcp.ObjectArguments(args)
	if err != nil {
		return nil, err
	}
	args = string(raw)
	return &preparedTool{Title: t.Server + " / " + t.OriginalName, permission: true, binding: t.Context(), run: func(ctx context.Context) toolOutput {
		result, err := t.Call(ctx, args)
		if err != nil {
			return toolOutput{Output: "Error: " + err.Error(), Failed: true}
		}
		return projectMCPResult(result, vision)
	}}, nil
}

// projectMCPResult validates images at the presentation boundary. Transports
// retain raw blocks; text and rawOutput receive descriptors, never image bytes.
func projectMCPResult(result mcp.Result, vision bool) toolOutput {
	var text strings.Builder
	var images []media.Image
	var imageContent []acp.ToolCallContent
	imageBlocks := 0 // invalid images also consume the decode-attempt bound
	var structured bytes.Buffer
	if result.StructuredContent != nil {
		_ = json.Compact(&structured, result.StructuredContent)
	}
	usable, unsupported := false, false
	for _, raw := range result.Content {
		var tag struct {
			Type     string `json:"type"`
			MIMEType string `json:"mimeType"`
		}
		if json.Unmarshal(raw, &tag) != nil {
			return toolOutput{Output: "Error: invalid MCP content; the operation may have completed", Failed: true}
		}
		var part string
		switch tag.Type {
		case "image":
			imageBlocks++
			var v struct {
				Data string `json:"data"`
			}
			var img media.Image
			var err error
			switch {
			case !vision:
				err = errors.New("selected model does not advertise image input")
			case imageBlocks > media.MaxImages:
				err = fmt.Errorf("image count exceeds the limit of %d", media.MaxImages)
			case json.Unmarshal(raw, &v) != nil:
				err = errors.New("invalid image data field")
			default:
				img, err = media.New(tag.MIMEType, v.Data)
				if err == nil {
					err = media.CheckSet(append(images, img))
				}
			}
			if err != nil {
				part = fmt.Sprintf("[Unsupported MCP image omitted: %v]", err)
				unsupported = true
			} else {
				images = append(images, img)
				imageContent = append(imageContent, acp.ToolCallContent{Type: "content", Content: acp.ContentBlock{Type: acp.ContentImage, Data: v.Data, MimeType: img.MIME()}})
				part = fmt.Sprintf("[MCP image: %s, %dx%d pixels, %d bytes]", img.MIME(), img.Width(), img.Height(), img.Size())
				usable = true
			}
		case "text":
			var v struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &v) != nil {
				return toolOutput{Output: "Error: invalid MCP text result", Failed: true}
			}
			part = v.Text
			usable = true
			// Servers commonly serialize structuredContent in a text block for
			// older clients. Keep one JSON copy without hiding distinct prose
			// or decoding numbers through float64.
			var serialized bytes.Buffer
			if structured.Len() > 0 && json.Compact(&serialized, []byte(v.Text)) == nil && bytes.Equal(serialized.Bytes(), structured.Bytes()) {
				continue
			}
		case "resource_link":
			var v struct{ URI, Name string }
			if json.Unmarshal(raw, &v) != nil {
				return toolOutput{Output: "Error: invalid MCP resource link", Failed: true}
			}
			part = fmt.Sprintf("[%s](%s)", v.Name, v.URI)
			usable = true
		case "resource":
			var v struct {
				Resource struct {
					URI      string
					Text     *string
					MIMEType string `json:"mimeType"`
				}
			}
			if json.Unmarshal(raw, &v) != nil {
				return toolOutput{Output: "Error: invalid MCP resource", Failed: true}
			}
			if v.Resource.Text != nil {
				part = fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", v.Resource.URI, *v.Resource.Text)
				usable = true
			} else {
				part = "[Unsupported MCP blob resource omitted]"
				unsupported = true
			}
		default:
			part = fmt.Sprintf("[Unsupported MCP content %q (%s) omitted]", tag.Type, tag.MIMEType)
			unsupported = true
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(part)
	}
	contentText := text.String()
	if result.StructuredContent != nil {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString("structuredContent: ")
		text.Write(result.StructuredContent)
		usable = true
	}
	if unsupported && !usable {
		text.WriteString("\nThe remote operation may have completed; its output cannot be consumed. Do not repeat it automatically.")
	}
	if text.Len() == 0 {
		text.WriteString("(no output)")
	}
	output := text.String()
	if len(output) > 256<<10 {
		return toolOutput{Output: "Error: MCP result exceeds 256 KiB; the remote operation may have completed. Do not repeat it automatically.", Failed: true}
	}
	// The rawOutput cap applies to text/JSON/metadata, independently of the
	// validated image bounds. It must not contain a second base64 copy.
	raw, _ := jsonrpc.Marshal(struct {
		Content    []acp.ContentBlock `json:"content"`
		Structured json.RawMessage    `json:"structuredContent,omitempty"`
		IsError    bool               `json:"isError,omitempty"`
	}{[]acp.ContentBlock{acp.Text(contentText)}, result.StructuredContent, result.IsError})
	if len(raw) > 256<<10 {
		return toolOutput{Output: "Error: MCP result exceeds 256 KiB; the remote operation may have completed", Failed: true}
	}
	return toolOutput{Output: output, Failed: result.IsError || unsupported && !usable, Content: append([]acp.ToolCallContent{acp.ToolText(output)}, imageContent...), RawOutput: raw, Images: images}
}

func mcpConfigs(servers []acp.MCPServer) []mcp.Config {
	var configs []mcp.Config
	for _, s := range servers {
		if s.Type != "stdio" && s.Type != "http" {
			continue
		}
		c := mcp.Config{Type: s.Type, Name: s.Name, Command: s.Command, Args: s.Args, URL: s.URL, Env: map[string]string{}, Headers: map[string]string{}}
		for _, v := range s.Env {
			c.Env[v.Name] = v.Value
		}
		for _, h := range s.Headers {
			c.Headers[http.CanonicalHeaderKey(h.Name)] = h.Value
		}
		configs = append(configs, c)
	}
	return configs
}
