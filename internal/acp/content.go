package acp

import (
	"encoding/json"
	"errors"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// Content block types acplause understands. Others are carried opaquely.
const (
	ContentText         = "text"
	ContentImage        = "image"
	ContentResourceLink = "resource_link"
	ContentResource     = "resource"
)

// ContentBlock is an ACP content block. Blocks decoded from the wire keep
// their exact JSON, so echoing a user's prompt back preserves unknown types
// and fields; the parsed fields cover what the agent reads.
type ContentBlock struct {
	Type     string
	Text     string // text
	URI      string // resource_link
	Name     string // resource_link
	Data     string // image, base64-encoded
	MimeType string // image

	Resource *Resource // resource

	raw json.RawMessage
}

// Resource is the contents of an embedded resource: text when Text is set,
// otherwise a blob, whose data acplause never reads.
type Resource struct {
	URI      string  `json:"uri"`
	MimeType string  `json:"mimeType"`
	Text     *string `json:"text"` // a pointer: empty text is still text
}

// Text returns a text content block.
func Text(s string) ContentBlock { return ContentBlock{Type: ContentText, Text: s} }

// MarshalJSON implements json.Marshaler.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	switch b.Type {
	case ContentText:
		return jsonrpc.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	case ContentResourceLink:
		return jsonrpc.Marshal(struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
			Name string `json:"name"`
		}{b.Type, b.URI, b.Name})
	case ContentImage:
		return jsonrpc.Marshal(struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}{b.Type, b.Data, b.MimeType})
	}
	return nil, errors.New("acp: cannot encode content block of type " + b.Type)
}

// UnmarshalJSON implements json.Unmarshaler.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	if discriminator.Type == "" {
		return errors.New("content block type is required")
	}
	parsed := ContentBlock{Type: discriminator.Type}
	switch discriminator.Type {
	case ContentText:
		var fields struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.Text = fields.Text
	case ContentResourceLink:
		var fields struct {
			URI  string `json:"uri"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.URI, parsed.Name = fields.URI, fields.Name
	case ContentImage:
		var fields struct {
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.Data, parsed.MimeType = fields.Data, fields.MimeType
	case ContentResource:
		var fields struct {
			Resource json.RawMessage `json:"resource"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if fields.Resource != nil {
			if err := json.Unmarshal(fields.Resource, &parsed.Resource); err != nil {
				return err
			}
		}
	}
	parsed.raw = append(json.RawMessage(nil), data...)
	*b = parsed
	return nil
}

// PromptRequest is the params of session/prompt.
type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// Validate implements jsonrpc's params validation.
func (r PromptRequest) Validate() error {
	var prompt error
	if r.Prompt == nil {
		prompt = errors.New("prompt is required")
	}
	return errors.Join(requireSessionID(r.SessionID), prompt)
}

// PromptResponse acknowledges insertion of the user message.
type PromptResponse struct {
	MessageID string `json:"messageId"`
}
