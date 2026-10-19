package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

const (
	maxReadSize  = 1 << 20 // read_file and search_code skip larger files
	maxWriteSize = 5 << 20
	maxListDepth = 3
	maxListItems = 200
)

// ignored names noise that list_dir and search_code skip.
func ignored(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "target" || name == "__pycache__"
}

var readFileSpec = Spec{
	Name:        "read_file",
	Kind:        acp.ToolRead,
	Description: "Read a text file. Use it to examine source code, configuration or any text file in the workspace.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path, relative to the working directory or absolute (e.g. 'src/main.go')"}
		},
		"required": ["path"]
	}`),
}

type readFileArgs struct {
	Path string `json:"path"`
}

func (tb *Toolbox) readFile(args readFileArgs) (*Call, error) {
	t, err := tb.roots.resolve(args.Path)
	if err != nil {
		return nil, err
	}
	return &Call{
		Title:     "Read " + args.Path,
		Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			info, err := t.root.Stat(t.rel)
			switch {
			case err != nil:
				return "", err
			case info.IsDir():
				return "", fmt.Errorf("%s is a directory; use list_dir", args.Path)
			case info.Size() > maxReadSize:
				return "", fmt.Errorf("%s is too large (%d bytes, max %d)", args.Path, info.Size(), maxReadSize)
			}
			data, err := t.readFile(ctx, maxReadSize)
			if err != nil {
				return "", err
			}
			if !utf8.Valid(data) {
				return "", fmt.Errorf("%s is not a text file", args.Path)
			}
			return string(data), nil
		},
	}, nil
}

var listDirSpec = Spec{
	Name:        "list_dir",
	Kind:        acp.ToolRead,
	Description: fmt.Sprintf("List a directory as a tree, %d levels deep and up to %d entries. Hidden entries and build output are skipped.", maxListDepth, maxListItems),
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Directory path, relative to the working directory or absolute (default '.')"}
		}
	}`),
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (tb *Toolbox) listDir(args listDirArgs) (*Call, error) {
	dir := cmp.Or(args.Path, ".")
	t, err := tb.roots.resolve(dir)
	if err != nil {
		return nil, err
	}
	return &Call{
		Title:     "List " + dir,
		Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			var b strings.Builder
			items := 0
			var walk func(dir string, depth int) error
			walk = func(dir string, depth int) error {
				entries, err := fs.ReadDir(t.root.FS(), dir)
				if err != nil {
					return err
				}
				for _, e := range entries {
					if ignored(e.Name()) {
						continue
					}
					if items == maxListItems {
						return errListFull
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					items++
					indent := strings.Repeat("  ", depth)
					if !e.IsDir() {
						fmt.Fprintf(&b, "%s%s\n", indent, e.Name())
						continue
					}
					fmt.Fprintf(&b, "%s%s/\n", indent, e.Name())
					if depth+1 < maxListDepth {
						if err := walk(path.Join(dir, e.Name()), depth+1); err != nil {
							return err
						}
					}
				}
				return nil
			}
			err := walk(filepath.ToSlash(t.rel), 0)
			if errors.Is(err, errListFull) {
				fmt.Fprintf(&b, "... truncated at %d entries\n", maxListItems)
				err = nil
			}
			return b.String(), err
		},
	}, nil
}

var errListFull = errors.New("listing is full")

var writeFileSpec = Spec{
	Name:        "write_file",
	Kind:        acp.ToolEdit,
	Description: "Create or overwrite a file with the given content, creating parent directories. For small changes to an existing file prefer edit.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path, relative to the working directory or absolute"},
			"content": {"type": "string", "description": "The complete new content of the file"}
		},
		"required": ["path", "content"]
	}`),
}

type writeFileArgs struct {
	Path    string  `json:"path"`
	Content *string `json:"content"` // nil distinguishes missing content from an intentional empty file
}

func (tb *Toolbox) writeFile(args writeFileArgs) (*Call, error) {
	t, err := tb.roots.resolve(args.Path)
	if err != nil {
		return nil, err
	}
	if args.Content == nil {
		return nil, errors.New("content is required")
	}
	if len(*args.Content) > maxWriteSize {
		return nil, fmt.Errorf("content is too large (%d bytes, max %d)", len(*args.Content), maxWriteSize)
	}
	plan := func(ctx context.Context) (*acp.ToolCallContent, error) {
		old, err := t.readFile(ctx, maxWriteSize)
		var d acp.ToolCallContent
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, fs.ErrNotExist):
			d = fileDiff(t.abs, "", false, *args.Content)
		case err != nil: // unreadable or too large: the write decides
			d = acp.ToolDiff([]acp.DiffChange{{Operation: acp.DiffModify, Path: t.abs, FileType: acp.FileText}}, nil)
		default:
			d = fileDiff(t.abs, string(old), true, *args.Content)
		}
		return &d, nil
	}
	c := &Call{Title: "Write " + args.Path, Locations: []string{t.abs}, preview: plan}
	c.run = func(ctx context.Context) (string, error) {
		// Diff first, so the change is known before anything is written.
		d, err := plan(ctx)
		if err != nil {
			return "", err
		}
		c.diff = d
		if err := t.root.MkdirAll(filepath.Dir(t.rel), 0o755); err != nil {
			return "", err
		}
		if err := t.writeFile(ctx, []byte(*args.Content)); err != nil {
			return "", err
		}
		return fmt.Sprintf("Wrote %d bytes to %s", len(*args.Content), t.abs), nil
	}
	return c, nil
}

var editSpec = Spec{
	Name:        "edit",
	Kind:        acp.ToolEdit,
	Description: "Replace exactly one occurrence of old_text with new_text in an existing file. Fails if old_text occurs zero or several times; include enough context to make it unique.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path, relative to the working directory or absolute"},
			"old_text": {"type": "string", "description": "Exact text to replace; must occur exactly once"},
			"new_text": {"type": "string", "description": "Replacement text"}
		},
		"required": ["path", "old_text", "new_text"]
	}`),
}

type editArgs struct {
	Path    string  `json:"path"`
	OldText string  `json:"old_text"`
	NewText *string `json:"new_text"` // an explicit empty string deletes the matched text
}

func (tb *Toolbox) edit(args editArgs) (*Call, error) {
	t, err := tb.roots.resolve(args.Path)
	if err != nil {
		return nil, err
	}
	if err := required("old_text", args.OldText); err != nil {
		return nil, err
	}
	if args.NewText == nil {
		return nil, errors.New("new_text is required")
	}
	plan := func(ctx context.Context) (string, *acp.ToolCallContent, error) {
		data, err := t.readFile(ctx, 0)
		if err != nil {
			return "", nil, err
		}
		content := string(data)
		switch n := strings.Count(content, args.OldText); n {
		case 0:
			return "", nil, fmt.Errorf("old_text not found in %s; re-read the file and copy the exact text", args.Path)
		case 1:
		default:
			return "", nil, fmt.Errorf("old_text occurs %d times in %s; add context to make it unique", n, args.Path)
		}
		updated := strings.Replace(content, args.OldText, *args.NewText, 1)
		d := fileDiff(t.abs, content, true, updated)
		return updated, &d, nil
	}
	c := &Call{Title: "Edit " + args.Path, Locations: []string{t.abs}}
	c.preview = func(ctx context.Context) (*acp.ToolCallContent, error) {
		_, d, err := plan(ctx)
		return d, err
	}
	c.run = func(ctx context.Context) (string, error) {
		// Plan again: the file may have changed since the preview.
		updated, d, err := plan(ctx)
		if err != nil {
			return "", err
		}
		c.diff = d
		if err := t.writeFile(ctx, []byte(updated)); err != nil {
			return "", err
		}
		return "Edited " + t.abs, nil
	}
	return c, nil
}
