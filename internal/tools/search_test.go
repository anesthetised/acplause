package tools

import (
	"context"
	"fmt"
	"path/filepath"
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
	want.WriteString(filepath.FromSlash("sub/a.txt") + ":1:HIT " + strings.Repeat("界", 98) + "…\n")
	for n := 1; n < maxMatches; n++ {
		fmt.Fprintf(&want, "%s:%d:hit hit\n", filepath.FromSlash("sub/b.txt"), n)
	}
	fmt.Fprintf(&want, "... stopped after %d matches\n", maxMatches)
	if got != want.String() {
		t.Fatalf("got %q, want %q", got, want.String())
	}
}

func TestSearchCodePagination(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("a.txt", "HIT first\nplain\nhit second\n")
	w.write("b.txt", "Hit third")
	for _, tc := range []struct {
		name, options, want string
	}{
		{"first page", `"limit":2`, "a.txt:1:HIT first\na.txt:3:hit second\n... more matches; next offset: 2\n"},
		{"last page", `"offset":2,"limit":2`, "b.txt:1:Hit third\n... no more matches\n"},
		{"one match", `"offset":1,"limit":1`, "a.txt:3:hit second\n... more matches; next offset: 2\n"},
		{"exact end", `"offset":3`, `No more matches for "^hit" at offset 3`},
		{"beyond end", `"offset":100`, `No more matches for "^hit" at offset 100`},
		{"explicit zero", `"offset":0,"context_lines":0`, "a.txt:1:HIT first\na.txt:3:hit second\nb.txt:1:Hit third\n... no more matches\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := w.tb.Prepare("search_code", `{"pattern":"^hit","regex":true,"ignore_case":true,`+tc.options+`}`)
			if err != nil {
				t.Fatal(err)
			}
			if got := call.Run(context.Background()); got.Failed || got.Output != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchCodeContext(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("a.txt", "before\nhit one\nmiddle\nhit two\nafter\nlast")
	for _, tc := range []struct {
		name, args, want string
	}{
		{"overlapping windows", `{"pattern":"hit","context_lines":1}`, "a.txt-1-before\na.txt:2:hit one\na.txt-3-middle\na.txt:4:hit two\na.txt-5-after\n... no more matches\n"},
		{"next match in context", `{"pattern":"hit","limit":1,"context_lines":2}`, "a.txt-1-before\na.txt:2:hit one\na.txt-3-middle\na.txt-4-hit two\n... more matches; next offset: 1\n"},
		{"context after offset", `{"pattern":"hit","offset":1,"limit":1,"context_lines":1}`, "a.txt-3-middle\na.txt:4:hit two\na.txt-5-after\n... no more matches\n"},
		{"skipped match is only context", `{"pattern":"hit","offset":1,"limit":1,"context_lines":2}`, "a.txt-2-hit one\na.txt-3-middle\na.txt:4:hit two\na.txt-5-after\na.txt-6-last\n... no more matches\n"},
		{"extra match before trailing window ends", `{"pattern":"hit","limit":1,"context_lines":5}`, "a.txt-1-before\na.txt:2:hit one\na.txt-3-middle\na.txt-4-hit two\na.txt-5-after\na.txt-6-last\n... more matches; next offset: 1\n"},
		{"file edges", `{"pattern":"^before$|^last$","regex":true,"context_lines":1}`, "a.txt:1:before\na.txt-2-hit one\na.txt-5-after\na.txt:6:last\n... no more matches\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := w.tb.Prepare("search_code", tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got := call.Run(context.Background()); got.Failed || got.Output != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchCodePageBoundaries(t *testing.T) {
	for _, total := range []int{0, 1, 50, 51} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			w := newWorkspace(t, Options{})
			w.write("a.txt", strings.Repeat("hit\n", total))
			got := w.ok("search_code", map[string]any{"pattern": "hit", "limit": 50})
			var want strings.Builder
			for n := 1; n <= min(total, 50); n++ {
				fmt.Fprintf(&want, "a.txt:%d:hit\n", n)
			}
			switch {
			case total == 0:
				want.WriteString(`No more matches for "hit" at offset 0`)
			case total > 50:
				want.WriteString("... more matches; next offset: 50\n")
			default:
				want.WriteString("... no more matches\n")
			}
			if got != want.String() {
				t.Fatalf("got %q, want %q", got, want.String())
			}
			legacy := w.ok("search_code", map[string]string{"pattern": "hit"})
			if strings.Contains(legacy, "offset:") || (total == 50 && strings.Contains(legacy, "stopped")) {
				t.Fatalf("legacy output changed: %q", legacy)
			}
		})
	}
}

func TestSearchCodeRejectsInvalidPageOptions(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, field := range []string{"offset", "limit", "context_lines"} {
		for _, value := range []string{`-1`, `1.5`, `null`, `true`, `"1"`, `[]`, `{}`, `999999999999999999999999999999999`} {
			args := fmt.Sprintf(`{"pattern":"hit",%q:%s}`, field, value)
			if _, err := w.tb.Prepare("search_code", args); err == nil {
				t.Errorf("accepted invalid option: %s", args)
			}
		}
	}
	for _, args := range []string{`{"pattern":"hit","limit":0}`, `{"pattern":"hit","limit":51}`, `{"pattern":"hit","context_lines":6}`} {
		if _, err := w.tb.Prepare("search_code", args); err == nil {
			t.Errorf("accepted invalid range: %s", args)
		}
	}
}

// searchCancelContext makes cancellation during skipped lines deterministic,
// without timers or a large repository. A small file finishes reading first.
type searchCancelContext struct {
	context.Context
	checks int
	cancel context.CancelFunc
}

// Err triggers real context cancellation after enough checks to reach scanning.
func (c *searchCancelContext) Err() error {
	c.checks++
	if c.checks >= 100 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestSearchCodeCancellationWhileSkipping(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("a.txt", strings.Repeat("hit\n", 1000))
	call, err := w.prepare("search_code", map[string]any{"pattern": "hit", "offset": 999})
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx := &searchCancelContext{Context: base, cancel: cancel}
	if got := call.Run(ctx); !got.Failed || !strings.Contains(got.Output, "context canceled") || strings.Contains(got.Output, "a.txt:") {
		t.Fatalf("cancelled skip: %+v", got)
	}
	// The first extra match ends traversal, rather than scanning to cancellation.
	call, err = w.prepare("search_code", map[string]any{"pattern": "hit", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	base, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx = &searchCancelContext{Context: base, cancel: cancel}
	if got := call.Run(ctx); got.Failed || got.Output != "a.txt:1:hit\n... more matches; next offset: 1\n" {
		t.Fatalf("bounded lookahead: %+v", got)
	}
}

func TestSearchCodeClippedContextWindow(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("a.txt", strings.Repeat("old\n", 20)+"before "+strings.Repeat("\u00e9", 170)+"\nhit\nafter "+strings.Repeat("\u00e9", 170))
	got := w.ok("search_code", map[string]any{"pattern": "hit", "context_lines": 5})
	var want strings.Builder
	for n := 17; n <= 20; n++ {
		fmt.Fprintf(&want, "a.txt-%d-old\n", n)
	}
	want.WriteString("a.txt-21-before " + strings.Repeat("\u00e9", 146) + "\u2026\n")
	want.WriteString("a.txt:22:hit\n")
	want.WriteString("a.txt-23-after " + strings.Repeat("\u00e9", 147) + "\u2026\n... no more matches\n")
	if got != want.String() {
		t.Fatalf("got %q, want %q", got, want.String())
	}
}
