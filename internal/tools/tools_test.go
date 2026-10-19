package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type workspace struct {
	t     *testing.T
	cwd   string
	extra string
	tb    *Toolbox
}

func newWorkspace(t *testing.T, opts Options) *workspace {
	t.Helper()
	base := t.TempDir()
	w := &workspace{t: t, cwd: filepath.Join(base, "project"), extra: filepath.Join(base, "shared")}
	for _, dir := range []string{w.cwd, w.extra} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tb, err := New(w.cwd, []string{w.extra}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tb.Close() })
	w.tb = tb
	return w
}

func (w *workspace) write(rel, content string) string {
	w.t.Helper()
	p := filepath.Join(w.cwd, rel)
	if filepath.IsAbs(rel) {
		p = rel
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *workspace) prepare(name string, args any) (*Call, error) {
	w.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		w.t.Fatal(err)
	}
	return w.tb.Prepare(name, string(raw))
}

// run prepares and runs a call that must be valid.
func (w *workspace) run(name string, args any) Result {
	w.t.Helper()
	call, err := w.prepare(name, args)
	if err != nil {
		w.t.Fatalf("prepare %s: %v", name, err)
	}
	return call.Run(context.Background())
}

func (w *workspace) ok(name string, args any) string {
	w.t.Helper()
	r := w.run(name, args)
	if r.Failed {
		w.t.Fatalf("%s failed: %s", name, r.Output)
	}
	return r.Output
}

func (w *workspace) fails(name string, args any, want string) {
	w.t.Helper()
	r := w.run(name, args)
	if !r.Failed || !strings.Contains(r.Output, want) {
		w.t.Fatalf("%s: got %+v, want failure containing %q", name, r, want)
	}
}

func TestPrepareRejectsBadCalls(t *testing.T) {
	w := newWorkspace(t, Options{})
	tests := []struct{ name, args, want string }{
		{"nope", `{}`, `unknown tool "nope"`},
		{"read_file", `{"path":`, "invalid arguments for read_file"},
		{"read_file", `{"path":42}`, "invalid arguments for read_file"},
		{"read_file", `{}`, "path is required"},
		{"read_file", `{"path":"../outside.txt"}`, "outside the workspace"},
		{"read_file", `{"path":"/etc/passwd"}`, "outside the workspace"},
		{"edit", `{"path":"a","old_text":"","new_text":"x"}`, "old_text is required"},
		{"search_code", `{"pattern":"x","file_glob":"["}`, "invalid file_glob"},
		{"bash", ``, "command is required"},
	}
	for _, tt := range tests {
		if _, err := w.tb.Prepare(tt.name, tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s %s: got %v, want %q", tt.name, tt.args, err, tt.want)
		}
	}
}

func TestBashTimeoutDefaultsAndZero(t *testing.T) {
	defaults := Options{}.withDefaults()
	if *defaults.BashTimeout != 30*time.Minute {
		t.Fatalf("default bash timeout: %v", *defaults.BashTimeout)
	}
	w := newWorkspace(t, Options{BashTimeout: new(time.Duration(0))})
	if *w.tb.opts.BashTimeout != 0 {
		t.Fatal("explicit zero was replaced by a default")
	}
	if got := w.ok("bash", map[string]string{"command": "printf done"}); got != "done" {
		t.Fatalf("unlimited command output: %q", got)
	}
}

func TestSandbox(t *testing.T) {
	w := newWorkspace(t, Options{})
	outside := w.write(filepath.Join(filepath.Dir(w.cwd), "secret.txt"), "secret")
	if err := os.Symlink(outside, filepath.Join(w.cwd, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(w.cwd), filepath.Join(w.cwd, "up")); err != nil {
		t.Fatal(err)
	}
	w.write(filepath.Join(w.extra, "lib.go"), "package lib")
	w.write("inside.txt", "inside")

	w.fails("read_file", map[string]string{"path": "link.txt"}, "escapes")
	w.fails("read_file", map[string]string{"path": "up/secret.txt"}, "escapes")
	w.fails("write_file", map[string]string{"path": "up/new.txt", "content": "x"}, "escapes")
	w.fails("write_file", map[string]string{"path": "link.txt", "content": "x"}, "escapes")
	if got := w.ok("read_file", map[string]string{"path": filepath.Join(w.extra, "lib.go")}); got != "package lib" {
		t.Fatalf("read from additional root: %q", got)
	}
	if got := w.ok("read_file", map[string]string{"path": filepath.Join(w.cwd, "inside.txt")}); got != "inside" {
		t.Fatalf("read by absolute path: %q", got)
	}
	if data, _ := os.ReadFile(outside); string(data) != "secret" {
		t.Fatalf("file outside the workspace changed: %q", data)
	}
}

func TestReadFile(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("src/main.go", "package main\n")
	w.write("blob.bin", "\xff\xfe\x00")
	w.write("big.txt", strings.Repeat("x", maxReadSize+1))

	call, err := w.prepare("read_file", map[string]string{"path": "src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if call.Title != "Read src/main.go" || call.Locations[0] != filepath.Join(w.cwd, "src/main.go") {
		t.Fatalf("unexpected call: %+v", call)
	}
	if got := call.Run(context.Background()); got.Failed || got.Output != "package main\n" {
		t.Fatalf("unexpected result: %+v", got)
	}
	w.fails("read_file", map[string]string{"path": "src"}, "is a directory")
	w.fails("read_file", map[string]string{"path": "blob.bin"}, "not a text file")
	w.fails("read_file", map[string]string{"path": "big.txt"}, "too large")
	w.fails("read_file", map[string]string{"path": "missing.txt"}, "no such file")
}

func TestReadFileRange(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("f.txt", "a\nb\nc\nd\ne\n")
	w.write("nonl.txt", "x\ny")
	w.write("empty.txt", "")
	tests := []struct {
		path string
		args map[string]any
		want string
	}{
		{"f.txt", map[string]any{"offset": 2, "limit": 2}, "[lines 2-3 of 5]\nb\nc\n"},
		{"f.txt", map[string]any{"offset": 4}, "[lines 4-5 of 5]\nd\ne\n"},
		{"f.txt", map[string]any{"limit": 1}, "[lines 1-1 of 5]\na\n"},
		{"f.txt", map[string]any{"offset": 5, "limit": 100}, "[lines 5-5 of 5]\ne\n"},
		{"f.txt", map[string]any{"offset": 2, "limit": math.MaxInt}, "[lines 2-5 of 5]\nb\nc\nd\ne\n"},
		{"nonl.txt", map[string]any{"offset": 2}, "[lines 2-2 of 2]\ny"},
		{"f.txt", map[string]any{}, "a\nb\nc\nd\ne\n"}, // no range, no header
	}
	for _, tc := range tests {
		tc.args["path"] = tc.path
		if got := w.run("read_file", tc.args); got.Failed || got.Output != tc.want {
			t.Errorf("%v: got %+v, want %q", tc.args, got, tc.want)
		}
	}
	w.fails("read_file", map[string]any{"path": "f.txt", "offset": 6}, "offset 6 is beyond the end of the file (5 lines)")
	w.fails("read_file", map[string]any{"path": "empty.txt", "offset": 1}, "(0 lines)")
}

func TestReadFileRangeRejectsInvalidArguments(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("f.txt", "a\n")
	for _, tc := range []struct{ args, want string }{
		{`{"path":"f.txt","offset":0}`, "offset must be at least 1"},
		{`{"path":"f.txt","offset":-3}`, "offset must be at least 1"},
		{`{"path":"f.txt","limit":0}`, "limit must be at least 1"},
		{`{"path":"f.txt","limit":-1}`, "limit must be at least 1"},
		{`{"path":"f.txt","offset":"2"}`, "invalid arguments for read_file"},
		{`{"path":"f.txt","limit":1.5}`, "invalid arguments for read_file"},
	} {
		if _, err := w.tb.Prepare("read_file", tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestListDir(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, f := range []string{"go.mod", "a/b/c/d/deep.txt", "a/x.go", ".git/HEAD", "node_modules/m/i.js"} {
		w.write(f, "")
	}
	want := "a/\n  b/\n    c/\n  x.go\ngo.mod\n"
	if got := w.ok("list_dir", map[string]string{}); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}

	for i := range maxListItems + 5 {
		w.write(fmt.Sprintf("many/f%03d", i), "")
	}
	got := w.ok("list_dir", map[string]string{"path": "many"})
	if lines := strings.Count(got, "\n"); lines != maxListItems+1 || !strings.Contains(got, "truncated") {
		t.Fatalf("got %d lines, want %d and a truncation note", lines, maxListItems+1)
	}
}

func TestSearchCode(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("main.go", "package main\nfunc needle() {}\n")
	w.write("notes.md", "needle in notes\n")
	w.write("node_modules/x/needle.js", "needle\n")
	w.write(filepath.Join(w.extra, "lib.go"), "// needle\n")
	if err := os.Symlink(filepath.Join(w.cwd, "main.go"), filepath.Join(w.cwd, "alias.go")); err != nil {
		t.Fatal(err)
	}

	if got, want := w.ok("search_code", map[string]string{"pattern": "needle"}), "main.go:2:func needle() {}\nnotes.md:1:needle in notes\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := w.ok("search_code", map[string]string{"pattern": "needle", "file_glob": "*.go"}), "main.go:2:func needle() {}\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := w.ok("search_code", map[string]string{"pattern": "needle", "path": w.extra}), filepath.Join(w.extra, "lib.go")+":1:// needle\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := w.ok("search_code", map[string]string{"pattern": "absent"}); got != `No matches for "absent"` {
		t.Fatalf("got %q", got)
	}

	w.write("many.txt", strings.Repeat("hit\n", maxMatches+10))
	got := w.ok("search_code", map[string]string{"pattern": "hit"})
	if strings.Count(got, "many.txt:") != maxMatches || !strings.Contains(got, "stopped after") {
		t.Fatalf("match limit not applied: %q", got)
	}
}

func TestWriteFileAndEdit(t *testing.T) {
	w := newWorkspace(t, Options{})
	path := filepath.Join(w.cwd, "pkg/new/file.go")
	if got := w.ok("write_file", map[string]string{"path": "pkg/new/file.go", "content": "one two two"}); got != "Wrote 11 bytes to "+path {
		t.Fatalf("got %q", got)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	w.fails("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "three", "new_text": "x"}, "not found")
	w.fails("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "two", "new_text": "x"}, "occurs 2 times")
	w.ok("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "one", "new_text": "1"})

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1 two two" {
		t.Fatalf("got %q, %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("edit changed permissions to %v", info.Mode().Perm())
	}
}

func TestWriteArgumentsRequireExplicitContent(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"write_file", `{"path":"a.txt"}`},
		{"write_file", `{"path":"a.txt","content":null}`},
		{"edit", `{"path":"a.txt","old_text":"keep"}`},
		{"edit", `{"path":"a.txt","old_text":"keep","new_text":null}`},
	} {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			p := w.write("a.txt", "keep")
			if call, err := w.tb.Prepare(tc.name, tc.args); err == nil {
				t.Errorf("accepted missing content: %+v", call.Run(context.Background()))
			}
			if data, err := os.ReadFile(p); err != nil || string(data) != "keep" {
				t.Fatalf("invalid call changed file: %q, %v", data, err)
			}
		})
	}
	w := newWorkspace(t, Options{})
	w.ok("write_file", map[string]string{"path": "empty.txt", "content": ""})
	w.write("a.txt", "keep")
	w.ok("edit", map[string]string{"path": "a.txt", "old_text": "keep", "new_text": ""})
	for _, name := range []string{"empty.txt", "a.txt"} {
		if data, err := os.ReadFile(filepath.Join(w.cwd, name)); err != nil || len(data) != 0 {
			t.Errorf("explicit empty content: %q, %v", data, err)
		}
	}
}

func TestBash(t *testing.T) {
	w := newWorkspace(t, Options{})
	if got := w.ok("bash", map[string]string{"command": "pwd; echo err >&2"}); got != w.cwd+"\nerr\n" && got != "/private"+w.cwd+"\nerr\n" {
		t.Fatalf("got %q", got)
	}
	w.fails("bash", map[string]string{"command": "echo partial; exit 3"}, "partial\n\nError: exit status 3")
	if got := w.ok("bash", map[string]string{"command": "true"}); got != "(no output)" {
		t.Fatalf("got %q", got)
	}
	capped := newWorkspace(t, Options{OutputLimit: 64})
	if got := capped.ok("bash", map[string]string{"command": "head -c 100 /dev/zero | tr '\\0' x"}); !strings.HasPrefix(got, strings.Repeat("x", 64)+"\n... (36 more bytes") {
		t.Fatalf("output not capped: %q", got)
	}
	call, _ := w.prepare("bash", map[string]string{"command": "  make test\n  && echo done"})
	if call.Title != "Run make test" {
		t.Fatalf("title: %q", call.Title)
	}
}

func TestInstructions(t *testing.T) {
	w := newWorkspace(t, Options{})
	ctx := context.Background()
	if got, err := w.tb.Instructions(ctx); got != "" || err != nil {
		t.Fatalf("missing file: %q, %v", got, err)
	}
	w.write("AGENTS.md", "Use tabs.\n")
	if got, err := w.tb.Instructions(ctx); got != "Use tabs." || err != nil {
		t.Fatalf("present file: %q, %v", got, err)
	}
	w.write("AGENTS.md", strings.Repeat("x", maxInstructionsSize+1))
	if _, err := w.tb.Instructions(ctx); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized file: %v", err)
	}
	w.write("AGENTS.md", "\xff\xfe")
	if _, err := w.tb.Instructions(ctx); err == nil {
		t.Fatal("binary file accepted")
	}
	if err := os.Remove(filepath.Join(w.cwd, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(w.cwd, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := w.tb.Instructions(ctx); err == nil {
		t.Fatal("directory accepted")
	}
}
