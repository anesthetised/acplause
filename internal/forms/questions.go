// Package forms validates ask_user form questions and their answers. It is
// split from internal/agent because it is stateless: it needs no session,
// permission or JSON-RPC state, only the wire types, so it is tested alone and
// the agent keeps just the elicitation round trip.
package forms

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

// field decodes an optional argument. An explicit null is invalid so it
// cannot silently acquire the omitted field's default.
func field[T any](raw json.RawMessage, name string) (v T, present bool, err error) {
	if raw == nil {
		return v, false, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil || string(raw) == "null" {
		return v, true, fmt.Errorf("%s has an invalid value", name)
	}
	return v, true, nil
}

func boundedString(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}

// ParseQuestions accepts only the bounded question surface from ADRs
// 0011/0017/0027/0028. Unknown fields remain compatible; the model cannot
// supply arbitrary schemas. The error is meant for the model, so the caller
// reports it as a failed tool call rather than a protocol error.
func ParseQuestions(args string) (acp.ElicitationRequest, error) {
	var input struct {
		Message     string `json:"message"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Questions   []struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Question string   `json:"question"`
			Options  []string `json:"options"`
			Choices  []struct {
				Value       string `json:"value"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"choices"`
			MultiSelect bool            `json:"multiSelect"`
			Type        json.RawMessage `json:"type"`
			Required    json.RawMessage `json:"required"`
			Default     json.RawMessage `json:"default"`
			MinLength   json.RawMessage `json:"minLength"`
			MaxLength   json.RawMessage `json:"maxLength"`
			Format      json.RawMessage `json:"format"`
			Minimum     json.RawMessage `json:"minimum"`
			Maximum     json.RawMessage `json:"maximum"`
			MinItems    json.RawMessage `json:"minItems"`
			MaxItems    json.RawMessage `json:"maxItems"`
			Pattern     json.RawMessage `json:"pattern"`
		} `json:"questions"`
	}
	req := acp.ElicitationRequest{Mode: "form", RequestedSchema: acp.ElicitationSchema{Type: "object", Properties: map[string]acp.ElicitationProperty{}}}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return req, fmt.Errorf("invalid ask_user arguments: %w", err)
	}
	if !boundedString(input.Message, 2000) || len(input.Questions) < 1 || len(input.Questions) > 3 {
		return req, errors.New("ask_user requires a message (1–2000 characters) and one to three questions")
	}
	if input.Title != "" && !boundedString(input.Title, 120) || input.Description != "" && !boundedString(input.Description, 2000) {
		return req, errors.New("form title or description exceeds its bounds")
	}
	req.Message = input.Message
	req.RequestedSchema.Title, req.RequestedSchema.Description = input.Title, input.Description
	for _, q := range input.Questions {
		if !boundedString(q.ID, 64) || !boundedString(q.Title, 120) || !boundedString(q.Question, 2000) || len(q.Options) > 10 || len(q.Choices) > 10 {
			return req, errors.New("invalid ask_user question: ID/title/question/options exceed their bounds or are empty")
		}
		if _, exists := req.RequestedSchema.Properties[q.ID]; exists {
			return req, fmt.Errorf("duplicate question ID %q", q.ID)
		}
		if len(q.Options) != 0 && len(q.Choices) != 0 {
			return req, fmt.Errorf("question %q sets both options and choices", q.ID)
		}
		seen := map[string]bool{}
		for _, option := range q.Options {
			if !boundedString(option, 200) || seen[option] {
				return req, fmt.Errorf("question %q has an empty, duplicate or oversized option", q.ID)
			}
			seen[option] = true
		}
		var titled []acp.ElicitationOption
		for _, c := range q.Choices {
			if !boundedString(c.Value, 200) || !boundedString(c.Title, 120) || utf8.RuneCountInString(c.Description) > 500 || seen[c.Value] {
				return req, fmt.Errorf("question %q has an empty, duplicate or oversized choice", q.ID)
			}
			seen[c.Value] = true
			titled = append(titled, acp.ElicitationOption{Const: c.Value, Title: c.Title, Description: c.Description})
		}
		kind, hasChoices := "string", len(seen) > 0
		if q.Type != nil {
			// An explicit null must not acquire the omitted field's default.
			kind = ""
			if err := json.Unmarshal(q.Type, &kind); err != nil {
				return req, fmt.Errorf("question %q type must be a string", q.ID)
			}
		}
		switch kind {
		case "string":
		case "integer", "number", "boolean":
			if hasChoices || q.MultiSelect {
				return req, fmt.Errorf("question %q choices require type string", q.ID)
			}
		default:
			return req, fmt.Errorf("question %q has an unsupported type", q.ID)
		}
		property := acp.ElicitationProperty{Type: kind, Title: q.Title, Description: q.Question, OneOf: titled}
		if len(q.Options) > 0 {
			property.Enum = q.Options // an empty list must not read as "no option is valid"
		}
		if q.MultiSelect {
			if !hasChoices {
				return req, fmt.Errorf("multiselect question %q requires options", q.ID)
			}
			property.Type, property.Enum, property.OneOf = "array", nil, nil
			property.Items = &acp.ElicitationEnumItems{Type: "string", Enum: q.Options}
			if titled != nil {
				property.Items = &acp.ElicitationEnumItems{AnyOf: titled}
			}
			n := len(seen)
			property.MaxItems = &n
		}
		if err := constrain(&property, len(seen), map[string]json.RawMessage{
			"minLength": q.MinLength, "maxLength": q.MaxLength, "format": q.Format, "minimum": q.Minimum,
			"maximum": q.Maximum, "minItems": q.MinItems, "maxItems": q.MaxItems, "pattern": q.Pattern,
		}); err != nil {
			return req, fmt.Errorf("question %q: %w", q.ID, err)
		}
		if q.Default != nil {
			if _, err := validateAnswer(property, q.Default); err != nil {
				return req, fmt.Errorf("question %q default %w", q.ID, err)
			}
			property.Default = q.Default
		}
		required, _, err := field[bool](q.Required, "required")
		if err != nil {
			return req, fmt.Errorf("question %q: %w", q.ID, err)
		}
		if q.Required == nil || required {
			req.RequestedSchema.Required = append(req.RequestedSchema.Required, q.ID)
		}
		req.RequestedSchema.Properties[q.ID] = property
	}
	return req, nil
}

// constrain applies the ADR 0028 constraint fields to a property. A field the
// property's type does not support, or a pattern (deferred, #159), fails the
// call rather than being silently dropped.
func constrain(p *acp.ElicitationProperty, offered int, raw map[string]json.RawMessage) error {
	if raw["pattern"] != nil {
		return errors.New("pattern is not supported")
	}
	numeric := p.Type == "integer" || p.Type == "number"
	plainString := p.Type == "string" && p.Enum == nil && p.OneOf == nil
	for name, ok := range map[string]bool{
		"minLength": plainString, "maxLength": plainString, "format": plainString,
		"minimum": numeric, "maximum": numeric, "minItems": p.Type == "array", "maxItems": p.Type == "array",
	} {
		if raw[name] != nil && !ok {
			return fmt.Errorf("%s does not apply to this question", name)
		}
	}
	count := func(name string) (*int, error) {
		n, present, err := field[uint32](raw[name], name)
		if !present || err != nil {
			return nil, err
		}
		v := int(n)
		return &v, nil
	}
	var err error
	if p.MinLength, err = count("minLength"); err != nil {
		return err
	}
	if p.MaxLength, err = count("maxLength"); err != nil {
		return err
	}
	if p.MinLength != nil && p.MaxLength != nil && *p.MinLength > *p.MaxLength {
		return errors.New("minLength exceeds maxLength")
	}
	format, present, err := field[string](raw["format"], "format")
	if err != nil || present && !boundedString(format, 64) {
		return errors.New("format must be a short string")
	}
	p.Format = format
	if p.Minimum, err = bound(p.Type, raw["minimum"], "minimum"); err != nil {
		return err
	}
	if p.Maximum, err = bound(p.Type, raw["maximum"], "maximum"); err != nil {
		return err
	}
	if p.Minimum != nil && p.Maximum != nil {
		// Compare integers exactly; float64 would conflate neighbours above 2^53.
		if p.Type == "integer" && inverted[int64](p.Minimum, p.Maximum) || p.Type == "number" && inverted[float64](p.Minimum, p.Maximum) {
			return errors.New("minimum exceeds maximum")
		}
	}
	if p.Type == "array" {
		if p.MinItems, err = count("minItems"); err != nil {
			return err
		}
		if max, err := count("maxItems"); err != nil {
			return err
		} else if max != nil {
			p.MaxItems = max
		}
		if p.MinItems != nil && *p.MinItems > *p.MaxItems || *p.MaxItems > offered {
			return errors.New("minItems/maxItems must satisfy 0 <= minItems <= maxItems <= number of choices")
		}
	}
	return nil
}

// bound normalizes an inclusive numeric bound to a JSON token of the field's
// type. Integer bounds are exact int64 values.
func bound(kind string, raw json.RawMessage, name string) (json.RawMessage, error) {
	switch kind {
	case "integer":
		n, present, err := field[int64](raw, name)
		if !present || err != nil {
			return nil, err
		}
		return json.RawMessage(strconv.FormatInt(n, 10)), nil
	case "number":
		n, present, err := field[float64](raw, name)
		if !present || err != nil {
			return nil, err
		}
		return json.RawMessage(strconv.FormatFloat(n, 'g', -1, 64)), nil
	}
	return nil, nil
}

// inverted reports whether the lower bound exceeds the upper one.
func inverted[T cmp.Ordered](lo, hi json.RawMessage) bool {
	var a, b T
	return json.Unmarshal(lo, &a) == nil && json.Unmarshal(hi, &b) == nil && a > b
}
