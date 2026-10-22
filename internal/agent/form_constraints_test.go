package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const twoChoices = `[{"value":"a","title":"Alpha","description":"First"},{"value":"b","title":"Beta"}]`

func oneQuestion(fields string) string {
	return `{"message":"Ask","questions":[{"id":"v","title":"V","question":"V?"` + fields + `}]}`
}

func TestFormConstraintArguments(t *testing.T) {
	for _, tt := range []struct {
		fields string
		valid  bool
	}{
		{`,"required":false`, true}, {`,"required":true`, true}, {`,"required":null`, false}, {`,"required":"no"`, false},
		{`,"minLength":0,"maxLength":3`, true}, {`,"minLength":2,"maxLength":1`, false}, {`,"minLength":-1`, false},
		{`,"maxLength":4294967296`, false}, {`,"minLength":1.5`, false}, {`,"maxLength":null`, false},
		{`,"format":"date"`, true}, {`,"format":"x-custom"`, true}, {`,"format":""`, false}, {`,"format":3`, false},
		{`,"pattern":"^a$"`, false}, {`,"pattern":null`, false},
		{`,"default":""`, true}, {`,"default":"d"`, true}, {`,"minLength":2,"default":"d"`, false},
		{`,"default":null`, false}, {`,"default":3`, false}, {`,"format":"date","default":"2026-02-30"`, false},
		{`,"type":"integer","minimum":0,"maximum":10,"default":0`, true}, {`,"type":"integer","minimum":11,"maximum":10`, false},
		{`,"type":"integer","minimum":1.5`, false}, {`,"type":"integer","default":11,"maximum":10`, false},
		{`,"type":"integer","minimum":9007199254740993,"maximum":9007199254740992`, false},
		{`,"type":"integer","minimum":-9223372036854775808,"maximum":9223372036854775807`, true},
		{`,"type":"integer","maximum":9223372036854775808`, false},
		{`,"type":"number","minimum":-1.5,"maximum":2.5,"default":0`, true}, {`,"type":"number","minimum":2,"maximum":1`, false},
		{`,"type":"number","default":"1"`, false}, {`,"type":"number","maximum":1e309`, false},
		{`,"type":"boolean","default":false`, true}, {`,"type":"boolean","default":"false"`, false},
		{`,"type":"boolean","minimum":0`, false}, {`,"type":"integer","minLength":1`, false}, {`,"type":"integer","format":"date"`, false},
		{`,"type":"number","minItems":0`, false}, {`,"minItems":0`, false}, {`,"maxItems":1`, false},
		{`,"choices":` + twoChoices, true}, {`,"choices":` + twoChoices + `,"default":"a"`, true},
		{`,"choices":` + twoChoices + `,"default":"Alpha"`, false}, {`,"choices":[]`, true},
		{`,"choices":[{"value":"a","title":"A"},{"value":"a","title":"B"}]`, false},
		{`,"choices":[{"value":"a","title":""}]`, false}, {`,"choices":[{"value":"","title":"A"}]`, false},
		{`,"choices":[{"value":"a","title":"A","description":"` + strings.Repeat("d", 501) + `"}]`, false},
		{`,"choices":[{"value":"a","title":"A","description":"` + strings.Repeat("é", 500) + `"}]`, true},
		{`,"choices":` + twoChoices + `,"options":["a"]`, false}, {`,"choices":` + twoChoices + `,"minLength":1`, false},
		{`,"choices":` + twoChoices + `,"format":"date"`, false}, {`,"type":"integer","choices":` + twoChoices, false},
		{`,"multiSelect":true,"choices":` + twoChoices + `,"minItems":1,"maxItems":2,"default":["a"]`, true},
		{`,"multiSelect":true,"choices":` + twoChoices + `,"minItems":0,"default":[]`, true},
		{`,"multiSelect":true,"options":["a","b"],"minItems":3`, false}, {`,"multiSelect":true,"options":["a","b"],"maxItems":3`, false},
		{`,"multiSelect":true,"options":["a","b"],"minItems":2,"maxItems":1`, false},
		{`,"multiSelect":true,"options":["a","b"],"minItems":1,"default":[]`, false},
		{`,"multiSelect":true,"options":["a","b"],"default":["a","a"]`, false}, {`,"multiSelect":true,"options":["a","b"],"default":["c"]`, false},
		{`,"multiSelect":true,"options":["a","b"],"default":"a"`, false}, {`,"multiSelect":true,"options":["a"],"minLength":1`, false},
		{`,"multiSelect":true,"choices":[]`, false},
	} {
		t.Run(tt.fields, func(t *testing.T) {
			_, err := parseQuestions(oneQuestion(tt.fields))
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, err=%v", tt.valid, err)
			}
		})
	}
	for name, args := range map[string]string{
		"title":            strings.Replace(oneQuestion(""), `"message"`, `"title":"`+strings.Repeat("t", 121)+`","message"`, 1),
		"description":      strings.Replace(oneQuestion(""), `"message"`, `"description":"`+strings.Repeat("d", 2001)+`","message"`, 1),
		"wrong title type": strings.Replace(oneQuestion(""), `"message"`, `"title":3,"message"`, 1),
	} {
		if _, err := parseQuestions(args); err == nil {
			t.Fatalf("%s: invalid form text accepted", name)
		}
	}
}

func TestFormConstraintWire(t *testing.T) {
	wire := func(questions string) string {
		req, err := parseQuestions(`{"message":"m","title":"Setup","description":"Details","questions":[` + questions + `]}`)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := jsonrpc.Marshal(req.RequestedSchema)
		if strings.Contains(string(encoded), `"required"`) {
			t.Errorf("all-optional form advertised required fields: %s", encoded)
		}
		return string(encoded)
	}
	text := wire(`{"id":"s","title":"S","question":"S?","default":"","required":false,"minLength":0,"maxLength":5,"format":"x-custom"},
	 {"id":"i","title":"I","question":"I?","type":"integer","minimum":0,"maximum":9223372036854775807,"default":0,"required":false},
	 {"id":"n","title":"N","question":"N?","type":"number","minimum":-0.5,"default":-0.5,"required":false}`) +
		wire(`{"id":"b","title":"B","question":"B?","type":"boolean","default":false,"required":false},
	 {"id":"o","title":"O","question":"O?","choices":`+twoChoices+`,"default":"b","required":false}`)
	for _, want := range []string{
		`"title":"Setup"`, `"description":"Details"`, `"default":""`, `"minLength":0`, `"maxLength":5`, `"format":"x-custom"`,
		`"minimum":0`, `"maximum":9223372036854775807`, `"default":0`, `"minimum":-0.5`, `"default":-0.5`, `"default":false`,
		`"oneOf":[{"const":"a","title":"Alpha","description":"First"},{"const":"b","title":"Beta"}]`, `"default":"b"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("schema lacks %s: %s", want, text)
		}
	}
	multi, err := parseQuestions(oneQuestion(`,"multiSelect":true,"choices":` + twoChoices + `,"minItems":0,"default":[]`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonrpc.Marshal(multi.RequestedSchema)
	for _, want := range []string{`"type":"array"`, `"items":{"anyOf":[{"const":"a","title":"Alpha","description":"First"},{"const":"b","title":"Beta"}]}`, `"minItems":0`, `"maxItems":2`, `"default":[]`, `"required":["v"]`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("multiselect schema lacks %s: %s", want, encoded)
		}
	}
}

func TestFormConstraintAnswers(t *testing.T) {
	for _, tt := range []struct {
		name, fields, answer string
		valid                bool
	}{
		{"unicode min", `,"minLength":2`, `"e\u0301"`, true}, {"unicode too short", `,"minLength":2`, `"\u00e9"`, false},
		{"emoji by code point", `,"maxLength":3`, `"😀😀😀"`, true}, {"emoji too long", `,"maxLength":3`, `"😀😀😀😀"`, false},
		{"empty allowed", ``, `""`, true}, {"empty too short", `,"minLength":1`, `""`, false},
		{"date", `,"format":"date"`, `"2026-02-28"`, true}, {"bad date", `,"format":"date"`, `"2026-02-30"`, false},
		{"date-time", `,"format":"date-time"`, `"2026-10-22T19:00:00+03:00"`, true}, {"bad date-time", `,"format":"date-time"`, `"2026-10-22 19:00"`, false},
		{"email", `,"format":"email"`, `"a@example.com"`, true}, {"display name", `,"format":"email"`, `"A <a@example.com>"`, false},
		{"not email", `,"format":"email"`, `"example.com"`, false},
		{"uri", `,"format":"uri"`, `"https://example.com/x?y=1"`, true}, {"no scheme", `,"format":"uri"`, `"example.com/x"`, false},
		{"space in uri", `,"format":"uri"`, `"https://example.com/a b"`, false},
		{"unknown format is annotation", `,"format":"hostname"`, `"not checked!"`, true},
		{"choice value", `,"choices":` + twoChoices, `"a"`, true}, {"choice title", `,"choices":` + twoChoices, `"Alpha"`, false},
		{"unlisted choice", `,"choices":` + twoChoices, `"c"`, false},
		{"integer at min", `,"type":"integer","minimum":-3,"maximum":3`, `-3`, true}, {"integer at max", `,"type":"integer","minimum":-3,"maximum":3`, `3`, true},
		{"integer below", `,"type":"integer","minimum":-3`, `-4`, false}, {"integer above", `,"type":"integer","maximum":3`, `4`, false},
		{"zero in range", `,"type":"integer","minimum":0,"maximum":0`, `0`, true},
		{"number at min", `,"type":"number","minimum":-0.5`, `-0.5`, true}, {"number below", `,"type":"number","minimum":-0.5`, `-0.51`, false},
		{"number above", `,"type":"number","maximum":0.5`, `0.5000001`, false}, {"integer token for number", `,"type":"number","maximum":2`, `2`, true},
		{"multi counts ok", `,"multiSelect":true,"options":["a","b","c"],"minItems":1,"maxItems":2`, `["a","c"]`, true},
		{"multi too few", `,"multiSelect":true,"options":["a","b","c"],"minItems":1`, `[]`, false},
		{"multi too many", `,"multiSelect":true,"options":["a","b","c"],"maxItems":2`, `["a","b","c"]`, false},
		{"multi empty allowed", `,"multiSelect":true,"choices":` + twoChoices, `[]`, true},
		{"multi title", `,"multiSelect":true,"choices":` + twoChoices, `["Alpha"]`, false},
		{"multi titled values", `,"multiSelect":true,"choices":` + twoChoices, `["b","a"]`, true},
		{"null", `,"required":false`, `null`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parseQuestions(oneQuestion(tt.fields))
			if err != nil {
				t.Fatal(err)
			}
			out := questionResult(req, acp.ElicitationResponse{Action: "accept", Content: json.RawMessage(`{"v":` + tt.answer + `}`)})
			if out.Failed == tt.valid {
				t.Fatalf("result=%+v, valid=%v", out, tt.valid)
			}
			if tt.valid && !strings.Contains(out.Output, `"v":`) {
				t.Fatalf("answer lost: %+v", out)
			}
		})
	}
}

func TestFormOptionalAnswers(t *testing.T) {
	args := `{"message":"m","questions":[
	 {"id":"need","title":"N","question":"N?"},
	 {"id":"flag","title":"F","question":"F?","type":"boolean","default":true,"required":false},
	 {"id":"count","title":"C","question":"C?","type":"integer","default":7,"required":false}]}`
	req, err := parseQuestions(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, content, want string
		valid               bool
	}{
		{"optional omitted", `{"need":"x"}`, `{"need":"x"}`, true},
		{"false and zero are answers", `{"need":"","flag":false,"count":0}`, `{"count":0,"flag":false,"need":""}`, true},
		{"required omitted", `{"flag":true}`, ``, false}, {"missing content", ``, ``, false}, {"null content", `null`, ``, false},
		{"unrequested key", `{"need":"x","other":1}`, ``, false},
		{"null optional", `{"need":"x","flag":null}`, ``, false}, {"wrong optional type", `{"need":"x","count":"7"}`, ``, false},
		{"array content", `[]`, ``, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := questionResult(req, acp.ElicitationResponse{Action: "accept", Content: json.RawMessage(tt.content)})
			if out.Failed == tt.valid || tt.valid && out.Output != tt.want {
				t.Fatalf("result=%+v, want %s (valid=%v)", out, tt.want, tt.valid)
			}
		})
	}
	// With no required fields, an accept without content means nothing answered.
	allOptional, err := parseQuestions(oneQuestion(`,"required":false,"default":"d"`))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{``, `null`, `{}`} {
		out := questionResult(allOptional, acp.ElicitationResponse{Action: "accept", Content: json.RawMessage(content)})
		if out.Failed || out.Output != `{}` {
			t.Fatalf("content %q: %+v (a default must not become an answer)", content, out)
		}
	}
	if out := questionResult(allOptional, acp.ElicitationResponse{Action: "decline"}); !out.Failed {
		t.Fatalf("declined form succeeded: %+v", out)
	}
}

func TestFormQuestionToolDescribesConstraints(t *testing.T) {
	var spec struct {
		Parameters struct {
			Properties map[string]json.RawMessage
		}
	}
	encoded, _ := json.Marshal(askUserTool.Function)
	if err := json.Unmarshal(encoded, &spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"title", "description", "questions"} {
		if spec.Parameters.Properties[name] == nil {
			t.Errorf("tool schema lacks %s", name)
		}
	}
	for _, name := range []string{"choices", "required", "default", "minLength", "maxLength", "format", "minimum", "maximum", "minItems", "maxItems"} {
		if !strings.Contains(string(encoded), fmt.Sprintf("%q", name)) {
			t.Errorf("tool schema lacks question field %s", name)
		}
	}
	if strings.Contains(string(encoded), `"pattern"`) {
		t.Error("tool schema advertises the unsupported pattern")
	}
}
