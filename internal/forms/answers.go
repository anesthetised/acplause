package forms

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

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

// Result is the outcome of a form for the model. Failed results carry an
// error message in Output; RawOutput is set only for accepted answers.
type Result struct {
	Output    string
	Failed    bool
	RawOutput json.RawMessage
}

// QuestionResult validates the client's response against the request the
// agent sent and builds the tool result for the model. Anything but a fully
// valid accept is a failed result: answers are never filled from defaults and
// never authorize tools.
func QuestionResult(req acp.ElicitationRequest, resp acp.ElicitationResponse) Result {
	fail := func(message string) Result { return Result{Output: "Error: " + message, Failed: true} }
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
	return Result{Output: string(encoded), RawOutput: encoded}
}
