package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveInstructionsChainAndFreshness(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write(filepath.Join(filepath.Dir(w.cwd), "AGENTS.md"), "Outside parent rules.")
	root := w.write("AGENTS.md", "  Root <&> rules.\n\n")
	child := w.write("src/AGENTS.md", "Child rules: caf\u00e9.\n")
	leaf := w.write("src/deep/AGENTS.md", "Leaf rules.\r\n")
	w.write("sibling/AGENTS.md", "Unrelated sibling rules.")
	w.write(filepath.Join(w.extra, "AGENTS.md"), "Unrelated root rules.")
	want := []Instruction{
		{Path: root, Text: "  Root <&> rules.\n\n"},
		{Path: child, Text: "Child rules: caf\u00e9.\n"},
		{Path: leaf, Text: "Leaf rules.\r\n"},
	}
	for _, target := range []string{"src/deep", filepath.Join(w.cwd, "src/deep")} {
		got, err := w.tb.ResolveInstructions(context.Background(), target)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%s: got %#v, %v; want %#v", target, got, err, want)
		}
	}
	w.write("src/AGENTS.md", "Changed child rules.")
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	want = []Instruction{{Path: root, Text: "  Root <&> rules.\n\n"}, {Path: child, Text: "Changed child rules."}}
	if got, err := w.tb.ResolveInstructions(context.Background(), "src/deep"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("stale rules: %#v, %v; want %#v", got, err, want)
	}
	if got, err := w.tb.Instructions(context.Background()); err != nil || got != "Root <&> rules." {
		t.Fatalf("legacy root reader changed: %q, %v", got, err)
	}
}

func TestResolveInstructionsSelectedRoots(t *testing.T) {
	w := newWorkspace(t, Options{})
	root := w.write("AGENTS.md", "Root.")
	nested := w.write("nested/AGENTS.md", "Nested.")
	leaf := w.write("nested/leaf/AGENTS.md", "Leaf.")
	extra := w.write(filepath.Join(w.extra, "AGENTS.md"), "Extra.")
	if got, err := w.tb.ResolveInstructions(context.Background(), w.extra); err != nil || !slices.Equal(got, []Instruction{{Path: extra, Text: "Extra."}}) {
		t.Fatalf("mixed unrelated roots: %#v, %v", got, err)
	}
	tb, err := New(w.cwd, []string{filepath.Dir(nested)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tb.Close() })
	want := []Instruction{{Path: root, Text: "Root."}, {Path: nested, Text: "Nested."}, {Path: leaf, Text: "Leaf."}}
	if got, err := tb.ResolveInstructions(context.Background(), filepath.Dir(leaf)); err != nil || !slices.Equal(got, want) {
		t.Fatalf("overlapping root selection changed: %#v, %v; want %#v", got, err, want)
	}
	inner, err := New(filepath.Dir(nested), []string{w.cwd}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	if got, err := inner.ResolveInstructions(context.Background(), "leaf"); err != nil || !slices.Equal(got, want[1:]) {
		t.Fatalf("relative lookup left cwd root: %#v, %v", got, err)
	}
}

func TestResolveInstructionsMissingAndEmptyFiles(t *testing.T) {
	w := newWorkspace(t, Options{})
	if got, err := w.tb.ResolveInstructions(context.Background(), "."); err != nil || len(got) != 0 {
		t.Fatalf("missing root file: %#v, %v", got, err)
	}
	source := w.write("child/AGENTS.md", "")
	w.write("child/leaf/keep", "not instructions")
	if got, err := w.tb.ResolveInstructions(context.Background(), "child/leaf"); err != nil || !slices.Equal(got, []Instruction{{Path: source, Text: ""}}) {
		t.Fatalf("missing ancestors or empty source: %#v, %v", got, err)
	}
}

func TestResolveInstructionsAbsoluteSourceLabels(t *testing.T) {
	w := newWorkspace(t, Options{})
	source := w.write("AGENTS.md", "Root.")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := New(relative, nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tb.Close() })
	if got, err := tb.ResolveInstructions(context.Background(), "."); err != nil || !slices.Equal(got, []Instruction{{Path: source, Text: "Root."}}) {
		t.Fatalf("ambiguous relative source label: %#v, %v", got, err)
	}
}

func TestResolveInstructionsDepthLimit(t *testing.T) {
	for _, depth := range []int{32, 33} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			w := newWorkspace(t, Options{})
			dir := filepath.Join(slices.Repeat([]string{"d"}, depth-1)...)
			source := w.write(filepath.Join(dir, "AGENTS.md"), "Leaf.")
			got, err := w.tb.ResolveInstructions(context.Background(), dir)
			if depth == 32 {
				if err != nil || !slices.Equal(got, []Instruction{{Path: source, Text: "Leaf."}}) {
					t.Fatalf("inclusive depth limit: %#v, %v", got, err)
				}
			} else if err == nil || len(got) != 0 || !strings.Contains(err.Error(), "32 directories") {
				t.Fatalf("excessive depth: %#v, %v", got, err)
			}
		})
	}
}

func TestResolveInstructionsByteLimits(t *testing.T) {
	w := newWorkspace(t, Options{})
	content := strings.Repeat("\u00e9", 16<<10) // exactly 32 KiB of valid UTF-8
	dir := "."
	for i := range 4 {
		w.write(filepath.Join(dir, "AGENTS.md"), content)
		if i < 3 {
			dir = filepath.Join(dir, "child")
		}
	}
	got, err := w.tb.ResolveInstructions(context.Background(), dir)
	if err != nil || len(got) != 4 {
		t.Fatalf("inclusive aggregate limit: %d sources, %v", len(got), err)
	}
	for _, source := range got {
		if source.Text != content {
			t.Fatal("source text was truncated or normalized")
		}
	}
	dir = filepath.Join(dir, "leaf")
	w.write(filepath.Join(dir, "AGENTS.md"), "")
	if got, err := w.tb.ResolveInstructions(context.Background(), dir); err != nil || len(got) != 5 {
		t.Fatalf("empty file exceeded aggregate byte limit: %d sources, %v", len(got), err)
	}
	bad := w.write(filepath.Join(dir, "AGENTS.md"), "x")
	if got, err := w.tb.ResolveInstructions(context.Background(), dir); err == nil || len(got) != 0 || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "aggregate") {
		t.Fatalf("excessive aggregate text: %#v, %v", got, err)
	}
	bad = w.write("AGENTS.md", strings.Repeat("x", (32<<10)+1))
	if got, err := w.tb.ResolveInstructions(context.Background(), "."); err == nil || len(got) != 0 || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("excessive file text: %#v, %v", got, err)
	}
}

func TestResolveInstructionsRejectsInvalidSources(t *testing.T) {
	for _, kind := range []string{"invalid UTF-8", "directory", "dangling symlink", "symlink loop", "outside-root symlink"} {
		t.Run(kind, func(t *testing.T) {
			w := newWorkspace(t, Options{})
			w.write("AGENTS.md", "Root.")
			w.write("child/keep", "not instructions")
			bad := filepath.Join(w.cwd, "child/AGENTS.md")
			var err error
			switch kind {
			case "invalid UTF-8":
				w.write("child/AGENTS.md", "\xff")
			case "directory":
				err = os.Mkdir(bad, 0o700)
			case "dangling symlink":
				err = os.Symlink("missing", bad)
			case "symlink loop":
				err = os.Symlink("AGENTS.md", bad)
			case "outside-root symlink":
				outside := w.write(filepath.Join(w.extra, "AGENTS.md"), "Unrelated root.")
				err = os.Symlink(outside, bad)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := w.tb.ResolveInstructions(context.Background(), "child"); err == nil || len(got) != 0 || !strings.Contains(err.Error(), bad) {
				t.Fatalf("invalid source returned partial rules: %#v, %v", got, err)
			}
		})
	}
}

func TestResolveInstructionsRejectsInvalidDirectories(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("plain.txt", "not a directory")
	w.write("child/keep", "not instructions")
	if err := os.Symlink(filepath.Join(w.cwd, "child"), filepath.Join(w.cwd, "absolute-link")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "../shared", "plain.txt", "absolute-link"} {
		if got, err := w.tb.ResolveInstructions(context.Background(), target); err == nil || len(got) != 0 {
			t.Errorf("invalid directory %q: %#v, %v", target, got, err)
		}
	}
	if _, err := w.tb.ResolveInstructions(context.Background(), "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory error: %v", err)
	}
	if err := os.Symlink(w.extra, filepath.Join(w.cwd, "escape")); err != nil {
		t.Fatal(err)
	}
	if got, err := w.tb.ResolveInstructions(context.Background(), "escape"); err == nil || len(got) != 0 {
		t.Fatalf("escaped selected root: %#v, %v", got, err)
	}
}

func TestResolveInstructionsLexicalSymlinkChain(t *testing.T) {
	w := newWorkspace(t, Options{})
	root := w.write("AGENTS.md", "Root.")
	w.write("src/AGENTS.md", "Physical ancestor, outside the requested lexical chain.")
	w.write("src/deep/AGENTS.md", "Leaf.")
	if err := os.Symlink(filepath.Join("src", "deep"), filepath.Join(w.cwd, "alias")); err != nil {
		t.Fatal(err)
	}
	want := []Instruction{{Path: root, Text: "Root."}, {Path: filepath.Join(w.cwd, "alias/AGENTS.md"), Text: "Leaf."}}
	if got, err := w.tb.ResolveInstructions(context.Background(), "alias"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("symlink lookup changed root selection or lexical scopes: %#v, %v", got, err)
	}
}

// instructionCancelContext cancels after the first source can be read, avoiding
// timing-dependent filesystem tests while checking that no partial chain leaks.
type instructionCancelContext struct {
	context.Context
	checks int
	cancel context.CancelFunc
}

// Err triggers real cancellation after several lookup/read checks.
func (c *instructionCancelContext) Err() error {
	c.checks++
	if c.checks >= 5 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestResolveInstructionsCancellation(t *testing.T) {
	w := newWorkspace(t, Options{})
	w.write("AGENTS.md", "Root.")
	w.write("child/AGENTS.md", "Child.")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := w.tb.ResolveInstructions(ctx, "child"); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("cancelled lookup: %#v, %v", got, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	during := &instructionCancelContext{Context: ctx, cancel: cancel}
	if got, err := w.tb.ResolveInstructions(during, "child"); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("cancellation returned partial rules: %#v, %v", got, err)
	}
}
