package tools

import (
	"bufio"
	"bytes"
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
	maxMatches    = 50
	maxMatchWidth = 300
)

var searchCodeSpec = Spec{
	Name:        "search_code",
	Kind:        acp.ToolSearch,
	Description: fmt.Sprintf("Search text files for a plain-text pattern (not a regex). Returns up to %d matching lines as path:line:text.", maxMatches),
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Text to search for"},
			"file_glob": {"type": "string", "description": "Only search files whose name matches this glob (e.g. '*.go')"},
			"path": {"type": "string", "description": "Directory to search, relative to the working directory or absolute (default '.')"}
		},
		"required": ["pattern"]
	}`),
}

type searchCodeArgs struct {
	Pattern  string `json:"pattern"`
	FileGlob string `json:"file_glob"`
	Path     string `json:"path"`
}

func (tb *Toolbox) searchCode(args searchCodeArgs) (*Call, error) {
	if err := required("pattern", args.Pattern); err != nil {
		return nil, err
	}
	if _, err := path.Match(args.FileGlob, ""); err != nil {
		return nil, fmt.Errorf("invalid file_glob: %v", err)
	}
	t, err := tb.roots.resolve(cmp.Or(args.Path, "."))
	if err != nil {
		return nil, err
	}
	return &Call{
		Title:     fmt.Sprintf("Search for %q", args.Pattern),
		Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			s := searcher{pattern: []byte(args.Pattern), glob: args.FileGlob, show: func(rel string) string {
				return tb.roots.display(t.root, filepath.FromSlash(rel))
			}}
			err := fs.WalkDir(t.root.FS(), filepath.ToSlash(t.rel), func(name string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return s.visit(ctx, tb.roots.within(t.root, filepath.FromSlash(name)), d)
			})
			if errors.Is(err, errSearchFull) {
				fmt.Fprintf(&s.out, "... stopped after %d matches\n", maxMatches)
				err = nil
			}
			if err == nil && s.matches == 0 {
				return fmt.Sprintf("No matches for %q", args.Pattern), nil
			}
			return s.out.String(), err
		},
	}, nil
}

var errSearchFull = errors.New("enough matches")

type searcher struct {
	pattern []byte
	glob    string
	show    func(rel string) string
	out     strings.Builder
	matches int
}

func (s *searcher) visit(ctx context.Context, t target, d fs.DirEntry) error {
	name := filepath.ToSlash(t.rel)
	if name != "." && ignored(d.Name()) {
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}
	if !d.Type().IsRegular() {
		return nil // directories are walked; symlinks are never followed
	}
	if s.glob != "" {
		if ok, _ := path.Match(s.glob, d.Name()); !ok {
			return nil
		}
	}
	if info, err := d.Info(); err != nil || info.Size() > maxReadSize {
		return nil
	}
	data, err := t.readFile(ctx, maxReadSize)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil
	}

	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, maxReadSize+1)
	for n := 1; lines.Scan(); n++ {
		if !bytes.Contains(lines.Bytes(), s.pattern) {
			continue
		}
		if s.matches == maxMatches {
			return errSearchFull
		}
		s.matches++
		fmt.Fprintf(&s.out, "%s:%d:%s\n", s.show(name), n, clip(strings.TrimSpace(lines.Text()), maxMatchWidth))
	}
	return lines.Err()
}

func clip(s string, width int) string {
	if len(s) <= width {
		return s
	}
	cut := width
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
