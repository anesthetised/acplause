package tools

import (
	"strings"
	"testing"
)

func TestIssue57SearchLineSizeBoundary(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, tc := range []struct {
		name      string
		size      int
		newline   bool
		wantMatch bool
	}{
		{name: "below without newline", size: maxReadSize - 1, wantMatch: true},
		{name: "exact without newline", size: maxReadSize, wantMatch: true},
		{name: "exact with newline", size: maxReadSize, newline: true, wantMatch: true},
		{name: "above without newline", size: maxReadSize + 1},
		{name: "above with newline", size: maxReadSize + 1, newline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := "needle" + strings.Repeat("x", tc.size-len("needle"))
			if tc.newline {
				payload = "needle" + strings.Repeat("x", tc.size-len("needle")-1) + "\n"
			}
			w.write(tc.name+".txt", payload)
			r := w.run("search_code", map[string]string{"pattern": "needle", "path": tc.name + ".txt"})
			if r.Failed {
				t.Fatalf("search failed: %s", r.Output)
			}
			if tc.wantMatch && !strings.HasPrefix(r.Output, tc.name+".txt:1:needle") {
				t.Fatalf("boundary match missing: %q", r.Output)
			}
			if !tc.wantMatch && r.Output != `No matches for "needle"` {
				t.Fatalf("oversized file was not skipped: %q", r.Output)
			}
		})
	}
}
