package acp

import (
	"encoding/json"
	"errors"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

// Content block types acplause understands. Others are carried opaquely.
const (
	ContentText         = "text"
	ContentResourceLink = "resource_link"
	ContentResource     = "resource"
)

// ContentBlock is an ACP content block. Blocks decoded from the wire keep
// their exact JSON, so echoing a user's prompt back preserves unknown types
// and fields; the parsed fields cover what the agent reads.
type ContentBlock struct {
	Type string
	Text string // text
	URI  string // resource_link
	Name string // resource_link

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
	}
	return nil, errors.New("acp: cannot encode content block of type " + b.Type)
}

// UnmarshalJSON implements json.Unmarshaler.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var fields struct {
		Type string `json:"type"`
		Text string `json:"text"`
		URI  string `json:"uri"`
		Name string `json:"name"`

		Resource json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// Only resource blocks are held to the resource shape, so other and
	// future block types may carry a field of that name.
	var resource *Resource
	if fields.Type == ContentResource && fields.Resource != nil {
		if err := json.Unmarshal(fields.Resource, &resource); err != nil {
			return err
		}
	}
	if fields.Type == "" {
		return errors.New("content block type is required")
	}
	*b = ContentBlock{
		Type: fields.Type,
		Text: fields.Text,
		URI:  fields.URI,
		Name: fields.Name,

		Resource: resource,
		raw:      append(json.RawMessage(nil), data...),
	}
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
