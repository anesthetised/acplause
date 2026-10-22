package e2e

import (
	"slices"
	"testing"
)

// ADR 0029: bash offers the four permission kinds, a remembered decision is
// replayed without a request, and the wire shapes validate against the schema.
func TestRememberedShellPermissions(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{"allow_always", "reject_always"} {
		t.Run(choice, func(t *testing.T) {
			t.Parallel()
			llm := newMockLLM(t)
			llm.enqueue(callTool("bash", `{"command":"true"}`), reply("one"), callTool("bash", `{"command":"true"}`), reply("two"))
			a, sid, _ := started(t, llm, askingUser)

			a.accepted(a.prompt(sid, "run it"))
			req := a.permissionRequest()
			p := decodePermission(t, req)
			var kinds []string
			for _, o := range p.Options {
				kinds = append(kinds, o.Kind)
			}
			if want := []string{"allow_once", "allow_always", "reject_once", "reject_always"}; !slices.Equal(kinds, want) {
				t.Fatalf("options %v, want %v", kinds, want)
			}
			a.answer(req, selected(optionOfKind(t, req, choice)))
			first := a.untilIdle(sid)

			a.accepted(a.prompt(sid, "run it again"))
			second := a.untilIdle(sid) // no permission request: the harness would block on one
			want := "completed"
			if choice == "reject_always" {
				want = "failed"
			}
			status := func(us []update) string {
				var last string
				for _, u := range filter(us, "tool_call_update") {
					if u.Status == "completed" || u.Status == "failed" {
						last = u.Status
					}
				}
				return last
			}
			if status(first) != want || status(second) != want {
				t.Fatalf("statuses %q then %q, want %q", status(first), status(second), want)
			}
		})
	}
}
