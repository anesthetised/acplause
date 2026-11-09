package forms

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
)

func TestElicitationTypedArguments(t *testing.T) {
	for _, tt := range []struct {
		fields, want string
	}{
		{``, "string"}, {`,"type":"string"`, "string"},
		{`,"type":"integer"`, "integer"}, {`,"type":"number"`, "number"}, {`,"type":"boolean"`, "boolean"},
		{`,"type":"string","options":["a"],"multiSelect":true`, "array"},
		{`,"type":"integer","options":[],"multiSelect":false`, "integer"},
		{`,"type":"number","_future":true`, "number"},
		{`,"type":null`, ""}, {`,"type":""`, ""}, {`,"type":true`, ""},
		{`,"type":"array"`, ""}, {`,"type":"_future"`, ""},
		{`,"type":"integer","options":["a"]`, ""},
		{`,"type":"number","multiSelect":true`, ""},
		{`,"type":"boolean","options":["yes","no"]`, ""},
	} {
		t.Run(tt.fields, func(t *testing.T) {
			args := `{"message":"Ask","questions":[{"id":"value","title":"Value","question":"Value?"` + tt.fields + `}]}`
			req, err := ParseQuestions(args)
			if tt.want == "" {
				if err == nil {
					t.Fatal("invalid typed question accepted")
				}
				return
			}
			if err != nil || req.RequestedSchema.Properties["value"].Type != tt.want {
				t.Fatalf("schema=%+v err=%v, want %s", req.RequestedSchema, err, tt.want)
			}
		})
	}
}

func TestElicitationTypedAnswers(t *testing.T) {
	for _, tt := range []struct {
		kind, value string
		valid       bool
	}{
		{"integer", `0`, true}, {"integer", `-1`, true}, {"integer", `-0`, true},
		{"integer", `9223372036854775807`, true}, {"integer", `-9223372036854775808`, true},
		{"integer", `9223372036854775808`, false}, {"integer", `-9223372036854775809`, false},
		{"integer", `1.0`, false}, {"integer", `1e3`, false}, {"integer", `1.5`, false},
		{"integer", `"1"`, false}, {"integer", `true`, false}, {"integer", `null`, false},
		{"number", `0`, true}, {"number", `-0`, true}, {"number", `-0.25`, true},
		{"number", `1.2300e+2`, true}, {"number", `9223372036854775807`, true},
		{"number", `1.7976931348623157e308`, true}, {"number", `5e-324`, true},
		{"number", `1e309`, false}, {"number", `-1e309`, false}, {"number", `NaN`, false},
		{"number", `"1.5"`, false}, {"number", `false`, false}, {"number", `null`, false},
		{"boolean", `true`, true}, {"boolean", `false`, true},
		{"boolean", `"true"`, false}, {"boolean", `0`, false}, {"boolean", `null`, false},
		{"boolean", `[]`, false}, {"boolean", `{}`, false},
	} {
		t.Run(tt.kind+"/"+tt.value, func(t *testing.T) {
			req, err := ParseQuestions(fmt.Sprintf(`{"message":"Ask","questions":[{"id":"value","title":"Value","question":"Value?","type":%q}]}`, tt.kind))
			if err != nil {
				t.Fatal(err)
			}
			out := QuestionResult(req, acp.ElicitationResponse{Action: "accept", Content: json.RawMessage(`{"value":` + tt.value + `}`)})
			if out.Failed == tt.valid {
				t.Fatalf("result=%+v, valid=%v", out, tt.valid)
			}
			if tt.valid {
				want := `{"value":` + tt.value + `}`
				if out.Output != want || string(out.RawOutput) != want {
					t.Fatalf("lost answer type/precision: output=%s raw=%s, want %s", out.Output, out.RawOutput, want)
				}
			}
		})
	}
}
