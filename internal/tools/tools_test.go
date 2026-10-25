package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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
	outside, _ := json.Marshal(map[string]string{"path": filepath.Join(filepath.Dir(w.cwd), "outside.txt")})
	tests := []struct{ name, args, want string }{
		{"nope", `{}`, `unknown tool "nope"`},
		{"read_file", `{"path":`, "invalid arguments for read_file"},
		{"read_file", `{"path":42}`, "invalid arguments for read_file"},
		{"read_file", `{}`, "path is required"},
		{"read_file", `{"path":"../outside.txt"}`, "outside the workspace"},
		{"read_file", string(outside), "outside the workspace"},
		{"edit", `{"path":"a","old_text":"","new_text":"x"}`, "old_text is required"},
		{"search_code", `{"pattern":"x","file_glob":"["}`, "invalid file_glob"},
		{testShellName(), ``, "command is required"},
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
	if got := w.ok(testShellName(), map[string]string{"command": testShellCommand(`printf done`, `[Console]::Write('done')`)}); got != "done" {
		t.Fatalf("unlimited command output: %q", got)
	}
}

func TestSandbox(t *testing.T) {
	w := newWorkspace(t, Options{})
	outside := w.write(filepath.Join(filepath.Dir(w.cwd), "secret.txt"), "secret")
	shared := w.write(filepath.Join(w.extra, "lib.go"), "package lib")
	inside := w.write("inside.txt", "inside")
	for _, path := range []string{inside, filepath.ToSlash(inside)} {
		if got := w.ok("read_file", map[string]string{"path": path}); got != "inside" {
			t.Fatalf("absolute path %q: %q", path, got)
		}
	}
	if got := w.ok("read_file", map[string]string{"path": shared}); got != "package lib" {
		t.Fatalf("additional root: %q", got)
	}
	for _, path := range []string{outside, filepath.ToSlash(outside), "../secret.txt", filepath.FromSlash("../secret.txt")} {
		for _, name := range []string{"read_file", "write_file"} {
			if _, err := w.prepare(name, map[string]string{"path": path, "content": "changed"}); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
				t.Errorf("%s accepted outside path %q: %v", name, path, err)
			}
		}
	}
	t.Run("symlink escapes", func(t *testing.T) {
		w := &workspace{t: t, cwd: w.cwd, extra: w.extra, tb: w.tb}
		if err := testSymlink(t, outside, filepath.Join(w.cwd, "link.txt")); err != nil {
			t.Fatal(err)
		}
		if err := testSymlink(t, filepath.Dir(w.cwd), filepath.Join(w.cwd, "up")); err != nil {
			t.Fatal(err)
		}
		w.fails("read_file", map[string]string{"path": "link.txt"}, "escapes")
		w.fails("read_file", map[string]string{"path": "up/secret.txt"}, "escapes")
		w.fails("write_file", map[string]string{"path": "up/new.txt", "content": "x"}, "escapes")
		w.fails("write_file", map[string]string{"path": "link.txt", "content": "x"}, "escapes")
	})
	if data, _ := os.ReadFile(outside); string(data) != "secret" {
		t.Fatalf("outside file changed: %q", data)
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
	missing := "no such file"
	if runtime.GOOS == "windows" {
		missing = "cannot find the file"
	}
	w.fails("read_file", map[string]string{"path": "missing.txt"}, missing)
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
		{"empty.txt", map[string]any{"offset": 1, "limit": 50}, "[empty file]\n"},
		{"empty.txt", map[string]any{"limit": 50}, "[empty file]\n"},
	}
	for _, tc := range tests {
		tc.args["path"] = tc.path
		if got := w.run("read_file", tc.args); got.Failed || got.Output != tc.want {
			t.Errorf("%v: got %+v, want %q", tc.args, got, tc.want)
		}
	}
	w.fails("read_file", map[string]any{"path": "f.txt", "offset": 6}, "offset 6 is beyond the end of the file (5 lines)")
	w.fails("read_file", map[string]any{"path": "empty.txt", "offset": 2}, "(0 lines)")
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

func TestHiddenTraversal(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, name := range []string{
		".env", ".github/workflows/ci.yml", ".github/.settings", ".github/.nested/config",
		".git/HEAD", ".GIT/HEAD", ".github/.git/config", "nested/.git", "nested/.config", "visible.txt",
		"node_modules/pkg/data", "target/data", "__pycache__/data",
	} {
		w.write(name, "needle\n")
	}
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		want       string
	}{
		{"list default", "list_dir", map[string]any{}, "nested/\nvisible.txt\n"},
		{"list explicit hidden root", "list_dir", map[string]any{"path": ".github"}, "workflows/\n  ci.yml\n"},
		{"list hidden descendants", "list_dir", map[string]any{"path": ".github", "include_hidden": true}, ".nested/\n  config\n.settings\nworkflows/\n  ci.yml\n"},
		{"search default", "search_code", map[string]any{"pattern": "needle"}, "visible.txt:1:needle\n"},
		{"search explicit false", "search_code", map[string]any{"pattern": "needle", "include_hidden": false}, "visible.txt:1:needle\n"},
		{"search explicit hidden root", "search_code", map[string]any{"pattern": "needle", "path": ".github"}, ".github/workflows/ci.yml:1:needle\n"},
		{"search explicit nested hidden root", "search_code", map[string]any{"pattern": "needle", "path": ".github/.nested"}, ".github/.nested/config:1:needle\n"},
		{"search explicit dot file", "search_code", map[string]any{"pattern": "needle", "path": ".env"}, ".env:1:needle\n"},
		{"search hidden descendants", "search_code", map[string]any{"pattern": "needle", "include_hidden": true}, ".env:1:needle\n.github/.nested/config:1:needle\n.github/.settings:1:needle\n.github/workflows/ci.yml:1:needle\nnested/.config:1:needle\nvisible.txt:1:needle\n"},
		{"list hidden descendants from root", "list_dir", map[string]any{"include_hidden": true}, ".env\n.github/\n  .nested/\n    config\n  .settings\n  workflows/\n    ci.yml\nnested/\n  .config\nvisible.txt\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.tool == "search_code" {
				tc.want = filepath.FromSlash(tc.want)
			}
			if got := w.ok(tc.tool, tc.args); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Traversal policy does not block direct file reads.
	if got := w.ok("read_file", map[string]string{"path": ".git/HEAD"}); got != "needle\n" {
		t.Fatalf("direct read changed: %q", got)
	}
}

func TestHiddenTraversalRejectsGitPathsAndInvalidFlags(t *testing.T) {
	w := newWorkspace(t, Options{})
	for _, tool := range []string{"search_code", "list_dir"} {
		for _, name := range []string{".git", ".GIT", ".git/HEAD", "nested/.GiT", "nested/.git", ".github/.git/config", filepath.Join(w.extra, ".git")} {
			for _, hidden := range []bool{false, true} {
				_, err := w.prepare(tool, map[string]any{"path": name, "pattern": "needle", "include_hidden": hidden})
				if err == nil || !strings.Contains(err.Error(), ".git") {
					t.Errorf("%s accepted .git path %s (hidden=%v): %v", tool, name, hidden, err)
				}
			}
		}
		for _, value := range []string{`null`, `1`, `"true"`, `[]`, `{}`} {
			args := `{"pattern":"needle","include_hidden":` + value + `}`
			if _, err := w.tb.Prepare(tool, args); err == nil {
				t.Errorf("%s accepted invalid flag: %s", tool, args)
			}
		}
	}
}

func TestHiddenTraversalAdditionalRootsAndSymlinks(t *testing.T) {
	w := newWorkspace(t, Options{})
	hidden := filepath.Join(w.extra, ".github")
	w.write(filepath.Join(hidden, "ci.yml"), "needle\n")
	w.write(filepath.Join(hidden, ".settings"), "needle\n")
	w.write(filepath.Join(w.extra, ".git"), "needle\n") // worktree metadata file
	if got := w.ok("search_code", map[string]any{"pattern": "needle", "path": hidden}); got != filepath.Join(hidden, "ci.yml")+":1:needle\n" {
		t.Fatalf("explicit hidden additional-root path: %q", got)
	}
	if got := w.ok("list_dir", map[string]any{"path": hidden}); got != "ci.yml\n" {
		t.Fatalf("explicit hidden additional-root listing: %q", got)
	}
	want := filepath.Join(hidden, ".settings") + ":1:needle\n" + filepath.Join(hidden, "ci.yml") + ":1:needle\n"
	if got := w.ok("search_code", map[string]any{"pattern": "needle", "path": w.extra, "include_hidden": true}); got != want {
		t.Fatalf("hidden additional-root search: %q, want %q", got, want)
	}
	if got := w.ok("list_dir", map[string]any{"path": w.extra, "include_hidden": true}); got != ".github/\n  .settings\n  ci.yml\n" {
		t.Fatalf("worktree .git file was listed: %q", got)
	}
	outside := filepath.Join(filepath.Dir(w.cwd), "outside")
	w.write(filepath.Join(outside, "secret.txt"), "needle\n")
	if err := testSymlink(t, outside, filepath.Join(w.cwd, ".external")); err != nil {
		t.Fatal(err)
	}
	if got := w.ok("search_code", map[string]any{"pattern": "needle", "include_hidden": true}); got != `No matches for "needle"` {
		t.Fatalf("followed hidden symlink: %q", got)
	}
	if got := w.ok("list_dir", map[string]any{"include_hidden": true}); got != ".external\n" {
		t.Fatalf("followed hidden directory symlink: %q", got)
	}
	w.fails("search_code", map[string]any{"pattern": "needle", "path": ".external"}, "escapes")
	w.fails("list_dir", map[string]any{"path": ".external"}, "escapes")

	gitRoot := filepath.Dir(w.write(filepath.Join(w.extra, "repo/.git/HEAD"), "needle\n"))
	tb, err := New(w.cwd, []string{gitRoot}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tb.Close() })
	for _, tool := range []string{"search_code", "list_dir"} {
		args, err := json.Marshal(map[string]any{"pattern": "needle", "path": gitRoot, "include_hidden": true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tb.Prepare(tool, string(args)); err == nil || !strings.Contains(err.Error(), ".git") {
			t.Errorf("%s accepted .git as an additional root: %v", tool, err)
		}
	}
}

func TestHiddenTraversalKeepsLimits(t *testing.T) {
	w := newWorkspace(t, Options{})
	for i := range maxListItems + 5 {
		w.write(fmt.Sprintf(".many/.f%03d", i), "needle\n")
	}
	got := w.ok("list_dir", map[string]any{"path": ".many", "include_hidden": true})
	if strings.Count(got, "\n") != maxListItems+1 || !strings.Contains(got, "truncated") {
		t.Fatalf("hidden listing exceeded cap: %q", got)
	}
	got = w.ok("search_code", map[string]any{"pattern": "needle", "include_hidden": true})
	if strings.Count(got, ":1:needle\n") != maxMatches || !strings.Contains(got, "stopped after 50 matches") {
		t.Fatalf("hidden search exceeded cap: %q", got)
	}
	w.write(".tree/a/b/c/deep.txt", "needle\n")
	if got := w.ok("list_dir", map[string]any{"path": ".tree", "include_hidden": true}); got != "a/\n  b/\n    c/\n" {
		t.Fatalf("hidden listing changed depth: %q", got)
	}
}

func TestSearchCode(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("main.go", "package main\nfunc needle() {}\n")
	w.write("notes.md", "needle in notes\n")
	w.write("node_modules/x/needle.js", "needle\n")
	w.write(filepath.Join(w.extra, "lib.go"), "// needle\n")
	if err := testSymlink(t, filepath.Join(w.cwd, "main.go"), filepath.Join(w.cwd, "alias.go")); err != nil {
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
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && before.Mode().Perm() != 0o600 {
		t.Fatalf("chmod fixture: %v", before.Mode().Perm())
	}

	w.fails("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "three", "new_text": "x"}, "not found")
	w.fails("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "two", "new_text": "x"}, "occurs 2 times")
	w.ok("edit", map[string]string{"path": "pkg/new/file.go", "old_text": "one", "new_text": "1"})

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "1 two two" {
		t.Fatalf("got %q, %v", data, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("edit changed permissions to %v", info.Mode().Perm())
	}
}

func TestEditInvalidPreviewNeverWrites(t *testing.T) {
	for _, tc := range []struct{ name, original, old, new string }{
		{"invalid UTF-8", "one\xff", "one", "new"},
		{"oversized input", strings.Repeat("a", maxWriteSize) + "b", "b", "c"},
		{"oversized output", "one", "one", strings.Repeat("a", maxWriteSize+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			path := w.write("a", tc.original)
			c, err := w.prepare("edit", map[string]string{"path": "a", "old_text": tc.old, "new_text": tc.new})
			if err != nil {
				t.Fatal(err)
			}
			if preview, err := c.Preview(context.Background()); err == nil || preview != nil {
				t.Fatalf("invalid preview: %+v, %v", preview, err)
			}
			if got := c.Run(context.Background()); !got.Failed || got.Diff != nil {
				t.Fatalf("failed preview permitted a write: %+v", got)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != tc.original {
				t.Fatalf("file changed: %d bytes, %v", len(data), err)
			}
		})
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
	physicalCwd, err := filepath.EvalSymlinks(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.ok(testShellName(), map[string]string{"command": testShellCommand(`pwd; echo err >&2`, "[Console]::Write((Get-Location).Path + \"`n\"); [Console]::Error.Write(\"err`n\")")}); got != w.cwd+"\nerr\n" && got != physicalCwd+"\nerr\n" {
		t.Fatalf("got %q", got)
	}
	w.fails(testShellName(), map[string]string{"command": testShellCommand(`echo partial; exit 3`, "[Console]::Write(\"partial`n\"); exit 3")}, "partial\n\nError: exit status 3")
	if got := w.ok(testShellName(), map[string]string{"command": testShellCommand(`true`, `$null = 1`)}); got != "(no output)" {
		t.Fatalf("got %q", got)
	}
	capped := newWorkspace(t, Options{OutputLimit: 64})
	if got := capped.ok(testShellName(), map[string]string{"command": testShellCommand(`head -c 100 /dev/zero | tr '\0' x`, `[Console]::Write(('x' * 100))`)}); !strings.HasPrefix(got, strings.Repeat("x", 64)+"\n... (36 more bytes") {
		t.Fatalf("output not capped: %q", got)
	}
	call, _ := w.prepare(testShellName(), map[string]string{"command": "  make test\n  && echo done"})
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

func TestCancelledWriteNeverTruncates(t *testing.T) {
	w := newWorkspace(t, Options{})
	old, updated := strings.Repeat("o", 4<<20), strings.Repeat("n", 4<<20)
	for i := range 40 {
		w.write("f.txt", old)
		call, err := w.prepare("write_file", map[string]string{"path": "f.txt", "content": updated})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(time.Duration(i%8) * 250 * time.Microsecond)
			cancel()
		}()
		call.Run(ctx)
		cancel()
		data, err := os.ReadFile(filepath.Join(w.cwd, "f.txt"))
		if err != nil || (string(data) != old && string(data) != updated) {
			t.Fatalf("attempt %d left %d bytes: %v", i, len(data), err)
		}
	}
}

// testShellCommand keeps the same behavior under the platform's native shell.
func testShellCommand(unix, windows string) string {
	if runtime.GOOS == "windows" {
		return windows
	}
	return unix
}

func testShellName() string { return testShellCommand("bash", "powershell") }

// testSymlink skips only when Windows denies the fixture's creation privilege.
// Successful creation still exercises os.Root's full confinement assertions.
func testSymlink(t *testing.T, target, link string) error {
	t.Helper()
	err := os.Symlink(target, link)
	if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) {
		t.Skip("symlink fixture requires Developer Mode or SeCreateSymbolicLinkPrivilege")
	}
	return err
}
