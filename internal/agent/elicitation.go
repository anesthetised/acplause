package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
	"github.com/anesthetised/acplause/internal/llm"
)

var askUserTool = llm.FunctionTool("ask_user", "Ask the user one to three questions when you need input to continue. Set type to string (default), integer (signed 64-bit), number (finite floating-point), or boolean. For string questions, use options for a single choice, or omit options for text. Set multiSelect:true with string options to allow multiple choices, including an empty selection. Other types cannot use choices or multiselect. This requests information, never permission to perform an action. Never request secrets or credentials, including passwords, API keys, tokens, private keys, recovery codes or payment credentials. If credentials are needed, ask the user to configure them outside this conversation and report only whether setup is complete. Do not ask them to paste secrets into chat.", json.RawMessage(`{
 "type":"object","properties":{
  "message":{"type":"string","minLength":1,"maxLength":2000},
  "title":{"type":"string","maxLength":120},"description":{"type":"string","maxLength":2000},
  "questions":{"type":"array","minItems":1,"maxItems":3,"items":{
   "type":"object","properties":{
    "id":{"type":"string","minLength":1,"maxLength":64},
    "title":{"type":"string","minLength":1,"maxLength":120},
    "question":{"type":"string","minLength":1,"maxLength":2000},
    "type":{"type":"string","enum":["string","integer","number","boolean"],"description":"Answer type; omission means string."},
    "options":{"type":"array","maxItems":10,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":200}},
    "choices":{"type":"array","maxItems":10,"description":"Titled alternative to options; set one of them.","items":{"type":"object","properties":{
     "value":{"type":"string","minLength":1,"maxLength":200},"title":{"type":"string","minLength":1,"maxLength":120},"description":{"type":"string","maxLength":500}
    },"required":["value","title"]}},
    "multiSelect":{"type":"boolean","description":"Allow zero or more offered options; requires nonempty options or choices."},
    "required":{"type":"boolean","description":"Default true. An optional question may be left unanswered."},
    "default":{"description":"Value pre-filled for the user; must be valid for the question. Never used as an answer."},
    "minLength":{"type":"integer","minimum":0},"maxLength":{"type":"integer","minimum":0},
    "format":{"type":"string","maxLength":64,"description":"String question format: date, date-time, email or uri are validated."},
    "minimum":{"type":"number"},"maximum":{"type":"number"},
    "minItems":{"type":"integer","minimum":0},"maxItems":{"type":"integer","minimum":0}
   },"required":["id","title","question"]
  }}
 },"required":["message","questions"]
}`))

func (s *Session) supportsForms() bool {
	return s.deps.forms != nil && s.deps.forms.Load()
}

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

// parseQuestions accepts only the bounded question surface from ADRs
// 0011/0017/0027/0028. Unknown fields remain compatible; the model cannot
// supply arbitrary schemas.
func parseQuestions(args string) (acp.ElicitationRequest, error) {
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

// outside reports whether v violates an optional inclusive bound. Bounds were
// normalized by bound, so a decode failure cannot occur.
func outside[T cmp.Ordered](v T, lo, hi json.RawMessage) bool {
	var a, b T
	return lo != nil && json.Unmarshal(lo, &a) == nil && v < a || hi != nil && json.Unmarshal(hi, &b) == nil && v > b
}

// offeredValues lists the machine values a choice or multiselect field accepts.
func offeredValues(p acp.ElicitationProperty) []string {
	if p.Items != nil {
		if p.Items.AnyOf == nil {
			return p.Items.Enum
		}
		return optionValues(p.Items.AnyOf)
	}
	if p.OneOf != nil {
		return optionValues(p.OneOf)
	}
	return p.Enum
}

func optionValues(options []acp.ElicitationOption) []string {
	values := make([]string, len(options))
	for i, o := range options {
		values[i] = o.Const
	}
	return values
}

// validFormat checks the formats whose syntax the pinned docs define or that
// have an unambiguous standard form. Unknown formats are annotations only.
func validFormat(format, s string) bool {
	switch format {
	case "date":
		_, err := time.Parse(time.DateOnly, s)
		return err == nil
	case "date-time":
		// RFC 3339 allows lowercase t/z and a leap second, which time.Parse
		// accepts in neither form.
		s = strings.ToUpper(s)
		if _, err := time.Parse(time.RFC3339, s); err == nil {
			return true
		}
		if _, rest, ok := strings.Cut(s, "T"); ok {
			if i := strings.IndexAny(rest, "Z+-"); i >= 3 && rest[i-3:i] == ":60" {
				// Go cannot represent :60; the rest of the layout is checked as :59.
				_, err := time.Parse(time.RFC3339, strings.Replace(s, ":60"+rest[i:], ":59"+rest[i:], 1))
				return err == nil
			}
		}
		return false
	case "email":
		// ParseAddress unquotes a quoted local part, so the input cannot be
		// compared with the result; a bare addr-spec has no name or comment.
		a, err := mail.ParseAddress(s)
		return err == nil && a.Name == "" && !strings.ContainsAny(s, "<(")
	case "uri":
		u, err := url.Parse(s)
		return err == nil && u.Scheme != "" && !strings.ContainsFunc(s, unicode.IsSpace)
	}
	return true
}

// validateAnswer checks one raw answer against its property and returns the
// value to report to the model. It also validates model-supplied defaults, so
// a default can never be something the user could not have answered.
func validateAnswer(property acp.ElicitationProperty, value json.RawMessage) (any, error) {
	switch property.Type {
	case "integer":
		var answer *int64
		if json.Unmarshal(value, &answer) != nil || answer == nil {
			return nil, errors.New("must be a signed 64-bit integer")
		}
		if outside(*answer, property.Minimum, property.Maximum) {
			return nil, errors.New("is outside the allowed range")
		}
		// Keep the validated token, including int64 boundary values, exact.
		return value, nil
	case "number":
		var answer *float64
		if json.Unmarshal(value, &answer) != nil || answer == nil {
			return nil, errors.New("must be a finite number")
		}
		if outside(*answer, property.Minimum, property.Maximum) {
			return nil, errors.New("is outside the allowed range")
		}
		return value, nil
	case "boolean":
		var answer *bool
		if json.Unmarshal(value, &answer) != nil || answer == nil {
			return nil, errors.New("must be a boolean")
		}
		return *answer, nil
	case "array":
		var selected []string
		if json.Unmarshal(value, &selected) != nil || selected == nil {
			return nil, errors.New("must be an array of offered options")
		}
		offered := offeredValues(property)
		if len(selected) > *property.MaxItems || property.MinItems != nil && len(selected) < *property.MinItems {
			return nil, errors.New("has a disallowed number of options")
		}
		for i, option := range selected {
			if !slices.Contains(offered, option) || slices.Contains(selected[:i], option) {
				return nil, errors.New("contains an unknown or duplicate option")
			}
		}
		return selected, nil
	case "string":
		var answer *string // JSON null must not become an empty string
		if json.Unmarshal(value, &answer) != nil || answer == nil {
			return nil, errors.New("must be a string")
		}
		if offered := offeredValues(property); offered != nil && !slices.Contains(offered, *answer) {
			return nil, errors.New("is not an offered option")
		}
		if n := utf8.RuneCountInString(*answer); property.MinLength != nil && n < *property.MinLength || property.MaxLength != nil && n > *property.MaxLength {
			return nil, errors.New("has a disallowed length")
		}
		if !validFormat(property.Format, *answer) {
			return nil, fmt.Errorf("is not a valid %s", property.Format)
		}
		return *answer, nil
	}
	return nil, errors.New("has an unsupported field type")
}

func questionResult(req acp.ElicitationRequest, resp acp.ElicitationResponse) toolOutput {
	fail := func(message string) toolOutput { return toolOutput{Output: "Error: " + message, Failed: true} }
	if resp.Action != "accept" {
		return fail(fmt.Sprintf("user input was not accepted (action %q)", resp.Action))
	}
	// Omitted and null content both mean no answers; only required fields object.
	var raw map[string]json.RawMessage
	if len(resp.Content) > 0 && json.Unmarshal(resp.Content, &raw) != nil {
		return fail("answers must be an object of the requested fields")
	}
	for id := range raw {
		if _, ok := req.RequestedSchema.Properties[id]; !ok {
			return fail(fmt.Sprintf("answer %q was not requested", id))
		}
	}
	answers := map[string]any{}
	for _, id := range slices.Sorted(maps.Keys(req.RequestedSchema.Properties)) {
		value, ok := raw[id]
		if !ok {
			if slices.Contains(req.RequestedSchema.Required, id) {
				return fail(fmt.Sprintf("answer %q is required", id))
			}
			continue // unanswered: never filled from a default
		}
		answer, err := validateAnswer(req.RequestedSchema.Properties[id], value)
		if err != nil {
			return fail(fmt.Sprintf("answer %q %v", id, err))
		}
		answers[id] = answer
	}
	encoded, _ := jsonrpc.Marshal(answers)
	return toolOutput{Output: string(encoded), RawOutput: encoded}
}

// askUser waits only on the session worker. The form is connection-local:
// resume replays the pending tool, but never sends this request a second time.
func (s *Session) askUser(ctx context.Context, req acp.ElicitationRequest) toolOutput {
	s.setState(acp.StateUpdate{State: acp.StateRequiresAction})
	s.flush()
	var resp acp.ElicitationResponse
	err := s.deps.out.CallCancellable(ctx, acp.MethodElicitationCreate, req, &resp)
	if ctx.Err() != nil {
		return toolOutput{Output: "Cancelled by the user.", Failed: true}
	}
	s.setState(acp.StateUpdate{State: acp.StateRunning})
	if err != nil {
		return toolOutput{Output: fmt.Sprintf("Error: could not get user input: %v", err), Failed: true}
	}
	return questionResult(req, resp)
}
