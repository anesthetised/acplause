package tools

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/anesthetised/acplause/internal/acp"
)

func TestFileDiff(t *testing.T) {
	t.Parallel()
	numbered := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "l%d\n", i)
		}
		return b.String()
	}
	tests := []struct {
		name    string
		path    string
		old     string
		existed bool
		new     string
		op      string
		patch   string // empty means nil patch
	}{
		{
			name: "add", path: "/w/a.txt", new: "x\ny\n", op: acp.DiffAdd,
			patch: "diff --git /w/a.txt /w/a.txt\nnew file mode 100644\n--- /dev/null\n+++ /w/a.txt\n@@ -0,0 +1,2 @@\n+x\n+y\n",
		},
		{name: "add empty", path: "/w/a.txt", op: acp.DiffAdd},
		{name: "invalid UTF-8", path: "/w/a.txt", existed: true, old: "a\n", new: "\xff\n", op: acp.DiffModify},
		{
			name: "modify", path: "/w/a.txt", existed: true, old: "a\nb\nc\n", new: "a\nB\nc\n", op: acp.DiffModify,
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n",
		},
		{
			name: "empty old", path: "/w/a.txt", existed: true, new: "x\n", op: acp.DiffModify,
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -0,0 +1 @@\n+x\n",
		},
		{
			name: "empty new", path: "/w/a.txt", existed: true, old: "x\ny\n", op: acp.DiffModify,
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -1,2 +0,0 @@\n-x\n-y\n",
		},
		{
			name: "missing newline", path: "/w/a.txt", existed: true, old: "a\nb", new: "a\nb\n", op: acp.DiffModify,
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n",
		},
		{
			name: "two hunks", path: "/w/a.txt", existed: true, old: numbered(20), op: acp.DiffModify,
			new:   strings.Replace(strings.Replace(numbered(20), "l2\n", "L2\n", 1), "l19\n", "L19\n", 1),
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -1,5 +1,5 @@\n l1\n-l2\n+L2\n l3\n l4\n l5\n@@ -16,5 +16,5 @@\n l16\n l17\n l18\n-l19\n+L19\n l20\n",
		},
		{
			// Six unchanged lines between the changes: git merges them.
			name: "merged hunks", path: "/w/a.txt", existed: true, old: numbered(9), op: acp.DiffModify,
			new:   strings.Replace(strings.Replace(numbered(9), "l1\n", "L1\n", 1), "l8\n", "L8\n", 1),
			patch: "diff --git /w/a.txt /w/a.txt\n--- /w/a.txt\n+++ /w/a.txt\n@@ -1,9 +1,9 @@\n-l1\n+L1\n l2\n l3\n l4\n l5\n l6\n l7\n-l8\n+L8\n l9\n",
		},
		{
			name: "quoted path", path: "/w/q\"\\\t\xc3\xa9.txt", new: "x\n", op: acp.DiffAdd,
			patch: `diff --git "/w/q\"\\\t\303\251.txt" "/w/q\"\\\t\303\251.txt"` + "\nnew file mode 100644\n--- /dev/null\n" +
				`+++ "/w/q\"\\\t\303\251.txt"` + "\n@@ -0,0 +1 @@\n+x\n",
		},
		{
			name: "space in path", path: "/w/a b.txt", existed: true, old: "a\n", new: "b\n", op: acp.DiffModify,
			patch: "diff --git /w/a b.txt /w/a b.txt\n--- /w/a b.txt\t\n+++ /w/a b.txt\t\n@@ -1 +1 @@\n-a\n+b\n",
		},
		{
			name: "Windows drive path", path: `C:\work\a.txt`, new: "x\n", op: acp.DiffAdd,
			patch: `diff --git "C:\\work\\a.txt" "C:\\work\\a.txt"` + "\nnew file mode 100644\n--- /dev/null\n" +
				`+++ "C:\\work\\a.txt"` + "\n@@ -0,0 +1 @@\n+x\n",
		},
		{name: "identical", path: "/w/a.txt", existed: true, old: "a\n", new: "a\n", op: acp.DiffModify},
		{
			name: "too large", path: "/w/a.txt", existed: true, op: acp.DiffModify,
			old: strings.Repeat("a\n", 3000), new: strings.Repeat("b\n", 3000),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := fileDiff(tt.path, tt.old, tt.existed, tt.new)
			if got.Type != "diff" || len(got.Changes) != 1 ||
				got.Changes[0] != (acp.DiffChange{Operation: tt.op, Path: tt.path, FileType: acp.FileText}) {
				t.Fatalf("changes: %+v", got)
			}
			if tt.patch == "" {
				if got.Patch != nil {
					t.Fatalf("want nil patch, got %q", got.Patch.Text)
				}
				return
			}
			if got.Patch == nil || got.Patch.Format != acp.PatchGit || got.Patch.Text != tt.patch {
				t.Fatalf("patch:\n got %+v\nwant %q", got.Patch, tt.patch)
			}
		})
	}
}

// TestFileDiffAppliesWithGit checks random patches against git itself:
// applying each patch to the old file must produce the new one.
func TestFileDiffAppliesWithGit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir()) // git rejects paths beyond symlinks
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	content := func() string {
		var b strings.Builder
		for range r.IntN(40) {
			fmt.Fprintf(&b, "line %d\n", r.IntN(8))
		}
		if r.IntN(4) == 0 {
			b.WriteString("no newline")
		}
		return b.String()
	}
	names := []string{"plain.txt", "with space.txt", "ünï.txt"}
	if runtime.GOOS != "windows" {
		names = append(names, `quo"te.txt`)
	}
	for i := range 100 {
		path := filepath.Join(dir, names[i%len(names)])
		existed, old, updated := i%5 != 0, "", content()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if existed {
			old = content()
			if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		diffPath := path
		if runtime.GOOS == "windows" {
			// Git's patch paths are relative to the fixture's drive. The unit
			// table separately pins native Windows absolute-path quoting.
			diffPath = filepath.Base(path)
		}
		d := fileDiff(diffPath, old, existed, updated)
		if d.Patch == nil {
			continue // identical or empty: nothing to apply
		}
		patch := filepath.Join(dir, "change.patch")
		if err := os.WriteFile(patch, []byte(d.Patch.Text), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "apply", "-p0", "--unsafe-paths", "--directory=/", patch)
		cmd.Dir = "/"
		if runtime.GOOS == "windows" {
			cmd = exec.Command("git", "apply", "-p0", "--unsafe-paths", patch)
			cmd.Dir = dir
		}
		cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(dir, "no-repo"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("case %d: git apply: %v\n%s\npatch:\n%s", i, err, out, d.Patch.Text)
		}
		if got, _ := os.ReadFile(path); string(got) != updated {
			t.Fatalf("case %d: applying the patch gave\n%q\nwant\n%q", i, got, updated)
		}
	}
}
