package acp

import (
	"encoding/json"
	"testing"
)

func TestElicitationCapabilityNegotiation(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		form bool
	}{
		{`{}`, false}, {`{"capabilities":null}`, false}, {`{"capabilities":{}}`, false},
		{`{"capabilities":{"elicitation":null}}`, false},
		{`{"capabilities":{"elicitation":{}}}`, false},
		{`{"capabilities":{"elicitation":{"form":null}}}`, false},
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
