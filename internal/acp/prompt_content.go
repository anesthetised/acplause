package acp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// promptMetadata validates known metadata fields without projecting them.
// The original objects are echoed, so they must also be valid for clients.
type promptMetadata struct {
	Type        *string `json:"type,case:strict"`
	Annotations *struct {
		Audience     []*string                  `json:"audience,case:strict"`
		LastModified *string                    `json:"lastModified,case:strict"`
		Priority     *float64                   `json:"priority,case:strict"`
		Meta         map[string]json.RawMessage `json:"_meta,case:strict"`
	} `json:"annotations,case:strict"`
	Meta map[string]json.RawMessage `json:"_meta,case:strict"`
}

func (m promptMetadata) validate() error {
	if m.Type == nil || *m.Type == "" {
		return errors.New("content block type is required")
	}
	if a := m.Annotations; a != nil {
		if a.Priority != nil && (*a.Priority < 0 || *a.Priority > 1) {
			return errors.New("annotations.priority must be between 0 and 1")
		}
		for _, role := range a.Audience {
			if role == nil {
				return errors.New("annotations.audience entries must be strings")
			}
		}
	}
	return nil
}

// validatePrompt applies the prompt capability rules and checks the fields of
// the selected variant. Historical content is decoded by ContentBlock alone.
func (b ContentBlock) validatePrompt() error {
	switch b.Type {
	case ContentText:
		var fields struct {
			promptMetadata
			Text *string `json:"text,case:strict"`
		}
		if err := json.Unmarshal(b.raw, &fields); err != nil {
			return err
		}
		if fields.Text == nil {
			return errors.New("text is required")
		}
		return fields.validate()
	case ContentImage:
		var fields struct {
			promptMetadata
			Data     *string `json:"data,case:strict"`
			MimeType *string `json:"mimeType,case:strict"`
			URI      *string `json:"uri,case:strict"`
		}
		if err := json.Unmarshal(b.raw, &fields); err != nil {
			return err
		}
		if fields.Data == nil {
			return errors.New("data is required")
		}
		if fields.MimeType == nil {
			return errors.New("mimeType is required")
		}
		if fields.URI != nil {
			if err := validatePromptURI(fields.URI, "uri"); err != nil {
				return err
			}
		}
		return fields.validate()
	case ContentResourceLink:
		return validatePromptResourceLink(b.raw)
	case ContentResource:
		return validatePromptResource(b.raw)
	default:
		return fmt.Errorf("unsupported content block type %q", b.Type)
	}
}

func validatePromptResourceLink(raw json.RawMessage) error {
	var fields struct {
		promptMetadata
		URI         *string `json:"uri,case:strict"`
		Name        *string `json:"name,case:strict"`
		Title       *string `json:"title,case:strict"`
		Description *string `json:"description,case:strict"`
		MimeType    *string `json:"mimeType,case:strict"`
		Size        *uint64 `json:"size,case:strict"`
		Icons       []*struct {
			Src      *string                    `json:"src,case:strict"`
			MimeType *string                    `json:"mimeType,case:strict"`
			Sizes    []*string                  `json:"sizes,case:strict"`
			Theme    *string                    `json:"theme,case:strict"`
			Meta     map[string]json.RawMessage `json:"_meta,case:strict"`
		} `json:"icons,case:strict"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields.Name == nil {
		return errors.New("name is required")
	}
	if err := validatePromptURI(fields.URI, "uri"); err != nil {
		return err
	}
	for i, icon := range fields.Icons {
		if icon == nil {
			return fmt.Errorf("icons[%d] must be an object", i)
		}
		if err := validatePromptURI(icon.Src, "src"); err != nil {
			return fmt.Errorf("icons[%d]: %w", i, err)
		}
		for _, size := range icon.Sizes {
			if size == nil {
				return fmt.Errorf("icons[%d].sizes entries must be strings", i)
			}
		}
	}
	return fields.validate()
}

func validatePromptResource(raw json.RawMessage) error {
	var fields struct {
		promptMetadata
		Resource *struct {
			embeddedResourceFields
			Meta map[string]json.RawMessage `json:"_meta,case:strict"`
		} `json:"resource,case:strict"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	r := fields.Resource
	if r == nil {
		return errors.New("resource is required")
	}
	if err := validatePromptURI(r.URI, "resource.uri"); err != nil {
		return err
	}
	var text *string
	if decodeOptionalString(r.Text, &text) != nil || text == nil {
		var blob *string
		if decodeOptionalString(r.Blob, &blob) != nil || blob == nil {
			return errors.New("resource must contain a text or blob string")
		}
		decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(*blob))
		if _, err := io.Copy(io.Discard, decoder); err != nil {
			return errors.New("resource.blob must be valid base64")
		}
	}
	return fields.validate()
}

func validatePromptURI(uri *string, name string) error {
	if uri == nil {
		return fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(*uri)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("%s must be an absolute URI", name)
	}
	return nil
}
