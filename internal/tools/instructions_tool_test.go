package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
)

func TestGetInstructionsTool(t *testing.T) {
	w := newWorkspace(t, Options{})
	root := w.write("AGENTS.md", "Root rules.\n")
	child := w.write("src/AGENTS.md", " Child rules.\n")
	w.write("src/deep/file.go", "package deep")
	w.write("sibling/AGENTS.md", "Unrelated rules.")
	call, err := w.prepare("get_instructions", map[string]any{"path": "src/deep"})
	if err != nil {
		t.Fatal(err)
	}
	if w.tb.Kind("get_instructions") != acp.ToolRead || len(call.Locations) != 1 || call.Locations[0] != filepath.Join(w.cwd, "src/deep") {
		t.Fatalf("read tool metadata: %+v", call)
	}
	if preview, err := call.Preview(context.Background()); preview != nil || err != nil {
		t.Fatalf("read preview: %+v, %v", preview, err)
	}
	out := call.Run(context.Background())
	if out.Failed || !strings.Contains(out.Output, "Root rules.\n") || !strings.Contains(out.Output, " Child rules.\n") || strings.Contains(out.Output, "Unrelated") || strings.Index(out.Output, root) >= strings.Index(out.Output, child) {
		t.Fatalf("scoped output: %+v", out)
	}
	for _, part := range []string{"root-to-directory", "precedence", "subtree", "permission"} {
		if !strings.Contains(out.Output, part) {
			t.Errorf("missing %q in %s", part, out.Output)
		}
	}
	empty := w.ok("get_instructions", map[string]any{"path": w.extra})
	if !strings.Contains(empty, "No AGENTS.md") {
		t.Fatalf("empty lookup: %s", empty)
	}
	for _, args := range []string{`{}`, `{"path":null}`, `{"path":42}`, `{"path":""}`} {
		if _, err := w.tb.Prepare("get_instructions", args); err == nil {
			t.Errorf("accepted arguments %s", args)
		}
	}
	for _, path := range []string{"src/deep/file.go", "missing", "../outside"} {
		call, err := w.prepare("get_instructions", map[string]any{"path": path})
		if err != nil {
			continue
		}
		if got := call.Run(context.Background()); !got.Failed {
			t.Errorf("accepted target %s: %+v", path, got)
		}
	}
	w.write("src/AGENTS.md", string([]byte{0xff}))
	if got := call.Run(context.Background()); !got.Failed || strings.Contains(got.Output, "Root rules.") {
		t.Fatalf("partial chain leaked on failure: %+v", got)
	}
}
