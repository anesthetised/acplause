package forms

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
	"github.com/anesthetised/acplause/internal/jsonrpc"
)

const questionArgs = `{"message":"Choose <&>","questions":[{"id":"choice","title":"Choice","question":"Which?","options":["a","b"]},{"id":"detail","title":"Detail","question":"Why?"},{"id":"last","title":"Last","question":"Anything else?"}]}`

func TestElicitationMultiselect(t *testing.T) {
	args := strings.Replace(questionArgs, `"options":["a","b"]`, `"options":["a","b"],"multiSelect":true`, 1)
	req, err := ParseQuestions(args)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonrpc.Marshal(req.RequestedSchema)
	if !strings.Contains(string(encoded), `"type":"array"`) || !strings.Contains(string(encoded), `"items":{"type":"string","enum":["a","b"]}`) {
		t.Fatalf("multiselect not advertised: %s", encoded)
	}
	for _, tt := range []struct {
		answer string
		valid  bool
	}{
		{`["b","a"]`, true}, {`[]`, true}, {`["a"]`, true},
		{`null`, false}, {`"a"`, false}, {`["c"]`, false},
		{`["a","a"]`, false}, {`[null]`, false}, {`[1]`, false},
	} {
		t.Run(tt.answer, func(t *testing.T) {
			content := json.RawMessage(`{"choice":` + tt.answer + `,"detail":"<&>","last":""}`)
			out := QuestionResult(req, acp.ElicitationResponse{Action: "accept", Content: content})
			if out.Failed == tt.valid || tt.valid && !strings.Contains(out.Output, `"choice":`+tt.answer) {
				t.Fatalf("result: %+v", out)
			}
		})
	}
	if _, err := ParseQuestions(strings.Replace(args, `"options":["a","b"],`, "", 1)); err == nil {
		t.Fatal("multiselect without options accepted")
	}
	if _, err := ParseQuestions(strings.Replace(args, `"multiSelect":true`, `"multiSelect":"yes"`, 1)); err == nil {
		t.Fatal("malformed multiselect flag accepted")
	}
}

func TestElicitationArguments(t *testing.T) {
	options := make([]string, 10)
	for i := range options {
		options[i] = fmt.Sprintf("%d", i) + strings.Repeat("x", 199)
	}
	boundary, _ := jsonrpc.Marshal(map[string]any{"message": strings.Repeat("m", 2000), "questions": []any{map[string]any{"id": strings.Repeat("i", 64), "title": strings.Repeat("t", 120), "question": strings.Repeat("q", 2000), "options": options}}})
	tests := []struct {
		name, args string
		valid      bool
	}{
		{"three questions", questionArgs, true},
		{"inclusive bounds", string(boundary), true},
		{"future fields", strings.Replace(questionArgs, `"message":`, `"_future":true,"message":`, 1), true},
		{"invalid JSON", "{", false}, {"null", "null", false}, {"empty", "{}", false},
		{"four questions", strings.Replace(questionArgs, `"questions":[`, `"questions":[{"id":"four","title":"4","question":"4"},`, 1), false},
		{"duplicate IDs", strings.Replace(questionArgs, `"id":"detail"`, `"id":"choice"`, 1), false},
		{"duplicate options", strings.Replace(questionArgs, `["a","b"]`, `["a","a"]`, 1), false},
		{"empty option", strings.Replace(questionArgs, `["a","b"]`, `[""]`, 1), false},
		{"empty ID", strings.Replace(questionArgs, `"id":"choice"`, `"id":" "`, 1), false},
		{"long ID", strings.Replace(questionArgs, `"id":"choice"`, `"id":"`+strings.Repeat("x", 65)+`"`, 1), false},
		{"long title", strings.Replace(questionArgs, `"title":"Choice"`, `"title":"`+strings.Repeat("x", 121)+`"`, 1), false},
		{"long question", strings.Replace(questionArgs, `"question":"Which?"`, `"question":"`+strings.Repeat("x", 2001)+`"`, 1), false},
		{"long message", strings.Replace(questionArgs, `Choose <&>`, strings.Repeat("x", 2001), 1), false},
		{"long option", strings.Replace(questionArgs, `["a","b"]`, `["`+strings.Repeat("x", 201)+`"]`, 1), false},
		{"too many options", strings.Replace(questionArgs, `["a","b"]`, `["a","b","c","d","e","f","g","h","i","j","k"]`, 1), false},
		{"wrong type", strings.Replace(questionArgs, `"title":"Choice"`, `"title":3`, 1), false},
		{"unicode limits", strings.Replace(questionArgs, `"title":"Choice"`, `"title":"`+strings.Repeat("\u00e9", 120)+`"`, 1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ParseQuestions(tt.args)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v: %v", tt.valid, err)
			}
			if err == nil && (req.Mode != "form" || len(req.RequestedSchema.Required) != len(req.RequestedSchema.Properties)) {
				t.Fatalf("form: %+v", req)
			}
		})
	}
}

func TestElicitationAnswers(t *testing.T) {
	req, err := ParseQuestions(questionArgs)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, raw string
		valid     bool
	}{
		{"accepted", `{"action":"accept","content":{"choice":"a","detail":"<&>","last":""},"_future":true}`, true},
		{"declined", `{"action":"decline"}`, false}, {"cancelled", `{"action":"cancel"}`, false},
		{"unknown", `{"action":"_future","content":{"choice":"a","detail":"b","last":"c"}}`, false},
		{"missing action", `{"content":{}}`, false}, {"missing content", `{"action":"accept"}`, false},
		{"null content", `{"action":"accept","content":null}`, false},
		{"missing required", `{"action":"accept","content":{"choice":"a","detail":"b"}}`, false},
		{"extra", `{"action":"accept","content":{"choice":"a","detail":"b","last":"c","extra":"d"}}`, false},
		{"unknown key replaces required", `{"action":"accept","content":{"choice":"a","detail":"b","extra":"c"}}`, false},
		{"array content", `{"action":"accept","content":["a","b","c"]}`, false},
		{"enum mismatch", `{"action":"accept","content":{"choice":"c","detail":"b","last":"c"}}`, false},
		{"number", `{"action":"accept","content":{"choice":"a","detail":2,"last":"c"}}`, false},
		{"null answer", `{"action":"accept","content":{"choice":"a","detail":null,"last":"c"}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var resp acp.ElicitationResponse
			if err := json.Unmarshal([]byte(tt.raw), &resp); err != nil {
				t.Fatal(err)
			}
			out := QuestionResult(req, resp)
			if out.Failed == tt.valid {
				t.Fatalf("result: %+v", out)
			}
			if tt.valid && !strings.Contains(out.Output, `<&>`) {
				t.Fatal(out.Output)
			}
		})
	}
}
