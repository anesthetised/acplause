package acp

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"unicode/utf8"

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
// their exact JSON for echo and replay. The parsed fields cover what the
// agent reads. PromptRequest validates new submissions separately.
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
		Type string `json:"type,case:strict"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	if discriminator.Type == "" {
		// Historical content used case-insensitive decoding. Keep it readable;
		// prompt validation still requires a canonical discriminator.
		var legacy struct{ Type string }
		if err := json.Unmarshal(data, &legacy); err != nil {
			return err
		}
		if legacy.Type == "" {
			return errors.New("content block type is required")
		}
		discriminator.Type = legacy.Type
	}
	parsed := ContentBlock{Type: discriminator.Type}
	switch discriminator.Type {
	case ContentText:
		var fields struct {
			Text string `json:"text,case:strict"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.Text = fields.Text
	case ContentResourceLink:
		var fields struct {
			URI  string `json:"uri,case:strict"`
			Name string `json:"name,case:strict"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.URI, parsed.Name = fields.URI, fields.Name
	case ContentImage:
		var fields struct {
			Data     string `json:"data,case:strict"`
			MimeType string `json:"mimeType,case:strict"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		parsed.Data, parsed.MimeType = fields.Data, fields.MimeType
	case ContentResource:
		var fields struct {
			Resource json.RawMessage `json:"resource,case:strict"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if fields.Resource != nil {
			var resource *embeddedResourceFields
			if err := json.Unmarshal(fields.Resource, &resource); err != nil {
				return err
			}
			if resource != nil {
				var text *string
				if err := decodeOptionalString(resource.Text, &text); err != nil {
					var blob *string
					if decodeOptionalString(resource.Blob, &blob) != nil || blob == nil {
						return err
					}
					text = nil
				}
				parsed.Resource = &Resource{Text: text}
				if resource.URI != nil {
					parsed.Resource.URI = *resource.URI
				}
				if resource.MimeType != nil {
					parsed.Resource.MimeType = *resource.MimeType
				}
			}
		}
	}
	parsed.raw = append(json.RawMessage(nil), data...)
	*b = parsed
	return nil
}

// embeddedResourceFields keeps the two payload alternatives separate. A
// property belonging only to the other alternative is opaque.
type embeddedResourceFields struct {
	URI      *string         `json:"uri,case:strict"`
	MimeType *string         `json:"mimeType,case:strict"`
	Text     json.RawMessage `json:"text,case:strict"`
	Blob     json.RawMessage `json:"blob,case:strict"`
}

func decodeOptionalString(raw json.RawMessage, into **string) error {
	if raw == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// IdempotencyKey identifies one prompt within a session. The zero value is unkeyed.
type IdempotencyKey string

// UnmarshalJSON rejects malformed Unicode instead of silently merging distinct
// input strings at the replacement character.
func (k *IdempotencyKey) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*k = ""
		return nil
	}
	var value string
	if err := jsonv2.Unmarshal(data, &value); err != nil || value == "" || utf8.RuneCountInString(value) > 255 {
		return errors.New("idempotencyKey must be a string of 1 to 255 characters")
	}
	*k = IdempotencyKey(value)
	return nil
}

// PromptRequest is the params of session/prompt.
type PromptRequest struct {
	SessionID      string         `json:"sessionId"`
	Prompt         []ContentBlock `json:"prompt"`
	IdempotencyKey IdempotencyKey `json:"idempotencyKey,omitempty,case:strict"`
}

// Validate implements jsonrpc's params validation.
func (r PromptRequest) Validate() error {
	if r.Prompt == nil {
		return errors.Join(requireSessionID(r.SessionID), errors.New("prompt is required"))
	}
	if err := requireSessionID(r.SessionID); err != nil {
		return err
	}
	// Beta.4 permits accepting an empty array. Keep the existing behavior.
	for i, b := range r.Prompt {
		if err := b.validatePrompt(); err != nil {
			return fmt.Errorf("prompt[%d]: %w", i, err)
		}
	}
	return nil
}

// PromptResponse acknowledges insertion of the user message.
type PromptResponse struct {
	MessageID string `json:"messageId"`
}
