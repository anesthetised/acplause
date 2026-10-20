package e2e

import (
	"slices"
	"testing"
)

func TestGroupedModelOptions(t *testing.T) {
	t.Parallel()
	llm := newMockLLM(t, `{"data":[{"id":"z/model"},{"id":"a/model"},{"id":"local"}]}`)
	llm.enqueue(reply("Selected."))
	a := start(t, llm)
	a.initialize()
	cwd := t.TempDir()
	var res struct {
		SessionID     string
		ConfigOptions []struct {
			ConfigID, CurrentValue string
			Options                []struct {
				GroupID, Name string
				Options       []struct{ Value, Name string }
			}
		}
	}
	a.ok("session/new", map[string]any{"cwd": cwd}, &res)
	sid := res.SessionID
	check := func(selected string) {
		t.Helper()
		model := res.ConfigOptions[0]
		var groups, values []string
		for _, group := range model.Options {
			groups = append(groups, group.GroupID)
			for _, option := range group.Options {
				values = append(values, option.Value)
			}
		}
		wantGroups, wantValues := []string{"provider:a", "provider:z", "unqualified"}, []string{"a/model", "z/model", "local"}
		if selected == "" {
			wantGroups = append([]string{"selection"}, wantGroups...)
			wantValues = append([]string{""}, wantValues...)
		}
		if model.CurrentValue != selected || !slices.Equal(groups, wantGroups) || !slices.Equal(values, wantValues) {
			t.Fatalf("model options: %+v", model)
		}
	}
	check("")
	a.ok("session/set_config_option", map[string]any{"sessionId": sid, "configId": "model", "type": "id", "value": "z/model"}, &res)
	check("z/model")
	a.ok("session/resume", map[string]any{"sessionId": sid, "cwd": cwd}, &res)
	a.untilIdle(sid)
	check("z/model")
	a.accepted(a.prompt(sid, "use my selection"))
	a.untilIdle(sid)
	if req := llm.request(0); req.Model != "z/model" {
		t.Fatalf("selection changed: %+v", req)
	}
}
