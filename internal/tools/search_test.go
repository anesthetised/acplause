package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestSearchCodeMatchingOptions(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("lines.txt", "a.b\naxb\nA.B\nCaf\u00e9\nCAF\u00c9\nfoo foo\nbar\n\n")
	for _, tc := range []struct {
		name, args, want string
	}{
		{"literal default", `{"pattern":"a.b"}`, "lines.txt:1:a.b\n"},
		{"explicit defaults", `{"pattern":"a.b","regex":false,"ignore_case":false}`, "lines.txt:1:a.b\n"},
		{"literal invalid regex", `{"pattern":"[","ignore_case":true}`, `No matches for "["`},
		{"regex", `{"pattern":"^a.b$","regex":true}`, "lines.txt:1:a.b\nlines.txt:2:axb\n"},
		{"literal case folding", `{"pattern":"a.b","ignore_case":true}`, "lines.txt:1:a.b\nlines.txt:3:A.B\n"},
		{"unicode case folding", `{"pattern":"caf\u00e9","ignore_case":true}`, "lines.txt:4:Caf\u00e9\nlines.txt:5:CAF\u00c9\n"},
		{"combined options", `{"pattern":"^a.b$","regex":true,"ignore_case":true}`, "lines.txt:1:a.b\nlines.txt:2:axb\nlines.txt:3:A.B\n"},
		{"alternation once per line", `{"pattern":"foo|bar","regex":true}`, "lines.txt:6:foo foo\nlines.txt:7:bar\n"},
		{"empty matches once per line", `{"pattern":"^|$","regex":true}`, "lines.txt:1:a.b\nlines.txt:2:axb\nlines.txt:3:A.B\nlines.txt:4:Caf\u00e9\nlines.txt:5:CAF\u00c9\nlines.txt:6:foo foo\nlines.txt:7:bar\nlines.txt:8:\n"},
		{"line local", `{"pattern":"(?s)foo.*bar","regex":true}`, `No matches for "(?s)foo.*bar"`},
		{"unknown field", `{"pattern":"bar","regex":true,"future_option":{}}`, "lines.txt:7:bar\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := w.tb.Prepare("search_code", tc.args)
			if err != nil {
				t.Fatal(err)
			}
			got := call.Run(context.Background())
			if got.Failed || got.Output != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchCodeRejectsInvalidMatchingOptions(t *testing.T) {
	w := newWorkspace(t, Options{})
	if _, err := w.tb.Prepare("search_code", `{"pattern":"[","regex":true}`); err == nil || !strings.Contains(err.Error(), "invalid regex") {
		t.Fatalf("invalid regex: %v", err)
	}
	for _, field := range []string{"regex", "ignore_case"} {
		for _, value := range []string{`"true"`, `1`, `[]`, `{}`, `null`} {
			args := fmt.Sprintf(`{"pattern":"x",%q:%s}`, field, value)
			if _, err := w.tb.Prepare("search_code", args); err == nil {
				t.Errorf("accepted invalid option: %s", args)
			}
		}
	}
}

func TestSearchCodeRegexBoundsAndFilters(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("sub/a.txt", "HIT "+strings.Repeat("界", 110)+"\n")
	w.write("sub/b.txt", strings.Repeat("hit hit\n", maxMatches+1))
	w.write("sub/ignored.go", "hit\n")
	w.write("outside.txt", "hit\n")
	got := w.ok("search_code", map[string]any{"pattern": "^hit", "regex": true, "ignore_case": true, "path": "sub", "file_glob": "*.txt"})
	var want strings.Builder
	want.WriteString("sub/a.txt:1:HIT " + strings.Repeat("界", 98) + "…\n")
	for n := 1; n < maxMatches; n++ {
		fmt.Fprintf(&want, "sub/b.txt:%d:hit hit\n", n)
	}
	fmt.Fprintf(&want, "... stopped after %d matches\n", maxMatches)
	if got != want.String() {
		t.Fatalf("got %q, want %q", got, want.String())
	}
}
