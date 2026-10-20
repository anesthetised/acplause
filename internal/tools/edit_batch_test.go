package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
)

func batchArgs(path string, pairs ...string) map[string]any {
	edits := make([]map[string]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		edits = append(edits, map[string]string{"old_text": pairs[i], "new_text": pairs[i+1]})
	}
	return map[string]any{"path": path, "edits": edits}
}

func readBatchFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file = %q, %v; want %q", got, err, want)
	}
}

func TestEditBatchRetainsOneSequentialPreview(t *testing.T) {
	w := newWorkspace(t, Options{})
	path := w.write("a.txt", "one\ntwo\ncaf\u00e9\n")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := w.prepare("edit_batch", batchArgs("a.txt", "one", "ONE", "ONE", "first", "two\n", "", "caf\u00e9", "th\u00e9"))
	if err != nil {
		t.Fatal(err)
	}
	if w.tb.Kind("edit_batch") != acp.ToolEdit || !reflect.DeepEqual(c.Locations, []string{path}) {
		t.Fatalf("edit metadata: %+v", c)
	}
	preview, err := c.Preview(context.Background())
	if err != nil || preview == nil || preview.Patch == nil || len(preview.Changes) != 1 {
		t.Fatalf("preview: %+v, %v", preview, err)
	}
	if !strings.Contains(preview.Patch.Text, "+first\n+th\u00e9\n") || strings.Contains(preview.Patch.Text, "+ONE") {
		t.Fatalf("preview does not show final candidate: %s", preview.Patch.Text)
	}
	readBatchFile(t, path, "one\ntwo\ncaf\u00e9\n")
	result := c.Run(context.Background())
	if result.Failed || !reflect.DeepEqual(result.Diff, preview) {
		t.Fatalf("approved and actual diff differ: %+v", result)
	}
	readBatchFile(t, path, "first\nth\u00e9\n")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("batch changed file permissions: %v", err)
	}
	if got := c.Run(context.Background()); !got.Failed || !strings.Contains(got.Output, "preview") {
		t.Fatalf("reused approval: %+v", got)
	}
}

func TestEditBatchRequiresExplicitBoundedArguments(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, raw := range []string{
		`{}`, `{"path":null,"edits":[{"old_text":"a","new_text":"b"}]}`,
		`{"path":"a"}`, `{"path":"a","edits":null}`, `{"path":"a","edits":[]}`,
		`{"path":"a","edits":{}}`, `{"path":"a","edits":[null]}`,
		`{"path":"a","edits":[{"new_text":"b"}]}`, `{"path":"a","edits":[{"old_text":null,"new_text":"b"}]}`,
		`{"path":"a","edits":[{"old_text":"","new_text":"b"}]}`, `{"path":"a","edits":[{"old_text":42,"new_text":"b"}]}`,
		`{"path":"a","edits":[{"old_text":"a"}]}`, `{"path":"a","edits":[{"old_text":"a","new_text":null}]}`,
		`{"path":"a","edits":[{"old_text":"a","new_text":false}]}`,
		`{"path":"../outside","edits":[{"old_text":"a","new_text":"b"}]}`,
	} {
		if _, err := w.tb.Prepare("edit_batch", raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, n := range []int{50, 51} {
		pairs := make([]string, 0, n*2)
		for range n {
			pairs = append(pairs, "a", "a")
		}
		c, err := w.prepare("edit_batch", batchArgs("a", pairs...))
		if n == 51 {
			if err == nil {
				t.Fatal("accepted 51 edits")
			}
			continue
		}
		w.write("a", "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Preview(context.Background()); err != nil {
			t.Fatalf("rejected 50 edits: %v", err)
		}
	}
	// Future fields remain compatible; an explicit empty string deletes text.
	w.write("a", "a")
	c, err := w.tb.Prepare("edit_batch", `{"path":"a","future":true,"edits":[{"old_text":"a","new_text":"","future":true}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Preview(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Run(context.Background()); got.Failed {
		t.Fatal(got.Output)
	}
	readBatchFile(t, filepath.Join(w.cwd, "a"), "")
}

func TestEditBatchInvalidPreviewNeverWrites(t *testing.T) {
	for _, tc := range []struct {
		name, original string
		pairs          []string
	}{
		{"missing later match", "one two", []string{"one", "ONE", "missing", "new"}},
		{"ambiguous", "one one", []string{"one", "ONE"}},
		{"overlapping matches", "aaa", []string{"aa", "new"}},
		{"ambiguity introduced", "one two", []string{"one", "two", "two", "new"}},
		{"invalid UTF-8", "one\xff", []string{"one", "new"}},
		{"oversized input", strings.Repeat("a", maxWriteSize+1), []string{"a", "new"}},
		{"oversized output", "one", []string{"one", strings.Repeat("a", maxWriteSize+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			path := w.write("a", tc.original)
			c, err := w.prepare("edit_batch", batchArgs("a", tc.pairs...))
			if err != nil {
				t.Fatal(err)
			}
			if preview, err := c.Preview(context.Background()); err == nil || preview != nil {
				t.Fatalf("invalid preview: %+v, %v", preview, err)
			}
			if got := c.Run(context.Background()); !got.Failed || got.Diff != nil {
				t.Fatalf("failed preview permitted a write: %+v", got)
			}
			readBatchFile(t, path, tc.original)
		})
	}
}

func TestEditBatchPreviewAndStaleContentFailClosed(t *testing.T) {
	for _, kind := range []string{"no preview", "changed", "deleted", "directory", "oversized", "cancelled", "symlink escape"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			path := w.write("a", "one two")
			c, err := w.prepare("edit_batch", batchArgs("a", "one", "ONE"))
			if err != nil {
				t.Fatal(err)
			}
			if kind != "no preview" {
				if _, err := c.Preview(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := "one two"
			switch kind {
			case "changed":
				want = "one changed externally"
				w.write("a", want)
			case "oversized":
				want = strings.Repeat("a", maxWriteSize+1)
				w.write("a", want)
			case "deleted", "directory", "symlink escape":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "symlink escape" {
					outside := filepath.Join(t.TempDir(), "outside")
					if err := os.WriteFile(outside, []byte("one two"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { readBatchFile(t, outside, "one two") })
				}
			case "cancelled":
				cancel()
			}
			// A second preview still describes the originally retained candidate.
			if kind != "no preview" && kind != "cancelled" {
				if _, err := c.Preview(context.Background()); err != nil {
					t.Fatalf("retained preview reread a changed target: %v", err)
				}
			}
			got := c.Run(ctx)
			if !got.Failed || got.Diff != nil || (kind != "no preview" && kind != "cancelled" && !strings.Contains(got.Output, "re-read")) {
				t.Fatalf("call did not fail closed: %+v", got)
			}
			if kind == "deleted" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("deleted file recreated: %v", err)
				}
			} else if kind == "directory" {
				if info, err := os.Stat(path); err != nil || !info.IsDir() {
					t.Fatalf("directory changed: %v", err)
				}
			} else if kind != "symlink escape" {
				readBatchFile(t, path, want)
			}
			if kind == "changed" {
				w.write("a", "one two")
				if retry := c.Run(context.Background()); !retry.Failed {
					t.Fatal("restoring bytes reused invalidated approval")
				}
			}
		})
	}
}

func TestEditBatchExactFileBoundsAndTransientGrowth(t *testing.T) {
	w := newWorkspace(t, Options{})
	full := "head" + strings.Repeat("x", maxWriteSize-4)
	path := w.write("a", full)
	// Only input and final size are capped: a temporary expansion can be undone.
	c, err := w.prepare("edit_batch", batchArgs("a", "head", "expanded-head", "expanded-head", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Preview(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Run(context.Background()); got.Failed {
		t.Fatal(got.Output)
	}
	readBatchFile(t, path, "HEAD"+full[4:])
	// Validate the catalog's array bounds as well as runtime validation.
	for _, spec := range w.tb.Specs() {
		if spec.Name == "edit_batch" {
			var schema struct {
				Properties map[string]struct{ MinItems, MaxItems int }
			}
			if err := json.Unmarshal(spec.Parameters, &schema); err != nil || schema.Properties["edits"].MinItems != 1 || schema.Properties["edits"].MaxItems != 50 {
				t.Fatalf("unbounded edit schema: %s, %v", spec.Parameters, err)
			}
			return
		}
	}
	t.Fatal("edit_batch not offered")
}

func TestEditBatchInitialTargetsAndSandbox(t *testing.T) {
	w := newWorkspace(t, Options{})
	if err := os.Mkdir(filepath.Join(w.cwd, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	w.write(outside, "one")
	if err := os.Symlink(outside, filepath.Join(w.cwd, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"missing", "directory", "escape"} {
		c, err := w.prepare("edit_batch", batchArgs(target, "one", "new"))
		if err != nil {
			t.Fatal(err)
		}
		if d, err := c.Preview(context.Background()); err == nil || d != nil {
			t.Fatalf("accepted target %s: %+v, %v", target, d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(w.cwd, "missing")); !os.IsNotExist(err) {
		t.Fatalf("preview created missing file: %v", err)
	}
	readBatchFile(t, outside, "one")
	local := w.write("local", "one")
	if err := os.Symlink("local", filepath.Join(w.cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	extra := w.write(filepath.Join(w.extra, "a"), "one")
	for _, target := range []string{"alias", extra} {
		c, err := w.prepare("edit_batch", batchArgs(target, "one", "new"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Preview(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := c.Run(context.Background()); got.Failed {
			t.Fatal(got.Output)
		}
	}
	readBatchFile(t, local, "new")
	readBatchFile(t, extra, "new")
}
