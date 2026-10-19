package acp

import (
	"encoding/json"
	"testing"

	"github.com/anesthetised/acplause/internal/jsonrpc"
)

func TestPlanNotification(t *testing.T) {
	u := PlanUpdate{Plan: PlanItems{Type: "items", PlanID: "p1", Entries: []PlanEntry{
		{Content: "check < > &", Priority: "high", Status: "in_progress"},
	}}}
	got, err := jsonrpc.Marshal(SessionNotification{SessionID: "s1", Update: u})
	want := `{"sessionId":"s1","update":{"sessionUpdate":"plan_update","plan":{"type":"items","planId":"p1","entries":[{"content":"check < > &","priority":"high","status":"in_progress"}]}}}`
	if err != nil || string(got) != want {
		t.Fatalf("plan notification = %s, %v; want %s", got, err, want)
	}
}

func TestPlanUpdateRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`{"plan":{"type":"items","planId":"empty","entries":[]}}`,
		`{"plan":{"type":"items","planId":"p","entries":[{"content":"","priority":"_urgent","status":"paused","_meta":{"x":1},"future":true}],"_meta":null},"future":{}}`,
		`{"plan":{"type":"_custom","planId":"p","entries":{"different":"shape"},"payload":[1,2]},"_meta":{"x":true}}`,
		`{"plan":{"type":"future","planId":"p","entries":false}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var u PlanUpdate
			if err := json.Unmarshal([]byte(raw), &u); err != nil {
				t.Fatal(err)
			}
			got, err := jsonrpc.Marshal(u)
			if err != nil || string(got) != raw {
				t.Fatalf("round trip = %s, %v; want %s", got, err, raw)
			}
			if u.Plan.PlanID == "" || u.Plan.Type == "" {
				t.Fatalf("plan identity missing: %+v", u.Plan)
			}
			if u.Plan.Type == "items" && u.Plan.PlanID == "p" {
				if len(u.Plan.Entries) != 1 || u.Plan.Entries[0].Priority != "_urgent" || u.Plan.Entries[0].Status != "paused" {
					t.Fatalf("known entries were not parsed: %+v", u.Plan)
				}
			}
		})
	}
}

func TestPlanDecodeResetsParsedFields(t *testing.T) {
	var u PlanUpdate
	for _, tt := range []struct {
		raw     string
		entries int
	}{
		{`{"plan":{"type":"items","planId":"p","entries":[{"content":"x","priority":"high","status":"pending"}]}}`, 1},
		{`{"plan":{"type":"items","planId":"p","entries":[]}}`, 0},
		{`{"plan":{"type":"_future","planId":"p","entries":{}}}`, 0},
	} {
		if err := json.Unmarshal([]byte(tt.raw), &u); err != nil {
			t.Fatal(err)
		}
		if len(u.Plan.Entries) != tt.entries {
			t.Fatalf("reused update has %d entries, want %d", len(u.Plan.Entries), tt.entries)
		}
	}
	for _, raw := range []string{`[]`, `{"plan":{"type":"items","entries":false}}`} {
		if err := json.Unmarshal([]byte(raw), &u); err == nil {
			t.Fatalf("malformed known plan accepted: %s", raw)
		}
	}
}
