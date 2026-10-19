package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestElicitationCapabilityNegotiation(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		form bool
	}{
		{`{}`, false}, {`{"capabilities":null}`, false}, {`{"capabilities":{}}`, false},
		{`{"capabilities":true}`, false}, {`{"capabilities":[]}`, false},
		{`{"capabilities":"supported"}`, false}, {`{"capabilities":1}`, false},
		{`{"capabilities":{"elicitation":null}}`, false},
		{`{"capabilities":{"elicitation":{}}}`, false},
		{`{"capabilities":{"elicitation":true}}`, false},
		{`{"capabilities":{"elicitation":false}}`, false},
		{`{"capabilities":{"elicitation":[]}}`, false},
		{`{"capabilities":{"elicitation":"form"}}`, false},
		{`{"capabilities":{"elicitation":1}}`, false},
		{`{"capabilities":{"elicitation":{"form":null}}}`, false},
		{`{"capabilities":{"elicitation":{"form":true}}}`, false},
		{`{"capabilities":{"elicitation":{"form":false}}}`, false},
		{`{"capabilities":{"elicitation":{"form":[]}}}`, false},
		{`{"capabilities":{"elicitation":{"form":"supported"}}}`, false},
		{`{"capabilities":{"elicitation":{"form":1}}}`, false},
		{`{"capabilities":{"elicitation":{"url":{},"_future":{}}}}`, false},
		{`{"capabilities":{"elicitation":{"form":{}}}}`, true},
		{`{"capabilities":{"elicitation":{"form":{"_future":true}},"_future":{}}}`, true},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			var req InitializeRequest
			if err := json.Unmarshal([]byte(tt.raw), &req); err != nil {
				t.Fatal(err)
			}
			got := req.Capabilities.Elicitation != nil && req.Capabilities.Elicitation.Form != nil
			if got != tt.form {
				t.Fatalf("form support %v, want %v", got, tt.form)
			}
		})
	}
}

func TestElicitationCapabilitiesResetOnReuse(t *testing.T) {
	var req InitializeRequest
	for _, raw := range []string{
		`{"capabilities":{"elicitation":{"form":{}}}}`,
		`{"capabilities":{"elicitation":{"form":true}}}`,
		`{"capabilities":{"elicitation":{"form":{}}}}`,
		`{"capabilities":{"elicitation":false}}`,
		`{"capabilities":{"elicitation":{"form":{}}}}`,
		`{"capabilities":{}}`,
		`{"capabilities":{"elicitation":{"form":{}}}}`,
		`{"capabilities":[]}`,
	} {
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		want := strings.Contains(raw, `"form":{}`)
		got := req.Capabilities.Elicitation != nil && req.Capabilities.Elicitation.Form != nil
		if got != want {
			t.Fatalf("stale capability after %s: form=%v", raw, got)
		}
	}
}
