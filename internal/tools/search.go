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
	"regexp"
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
	Description: fmt.Sprintf("Search text files for a pattern, literal and case-sensitive by default. Supports opt-in regex and case-insensitive matching. Returns up to %d matching lines as path:line:text.", maxMatches),
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Text to search for, or a Go regular expression when regex is true"},
			"regex": {"type": "boolean", "description": "Treat pattern as a Go regular expression, matched per line (default false)"},
			"ignore_case": {"type": "boolean", "description": "Use Unicode case-insensitive matching (default false)"},
			"file_glob": {"type": "string", "description": "Only search files whose name matches this glob (e.g. '*.go')"},
			"path": {"type": "string", "description": "Directory to search, relative to the working directory or absolute (default '.')"}
		},
		"required": ["pattern"]
	}`),
}

type searchCodeArgs struct {
	Pattern    string       `json:"pattern"`
	Regex      optionalBool `json:"regex"`
	IgnoreCase optionalBool `json:"ignore_case"`
	FileGlob   string       `json:"file_glob"`
	Path       string       `json:"path"`
}

// optionalBool defaults to false when omitted, but rejects null, which a plain
// bool's JSON decoder would silently accept as an omitted value.
type optionalBool bool

// UnmarshalJSON accepts explicit JSON booleans only.
func (b *optionalBool) UnmarshalJSON(raw []byte) error {
	switch string(raw) {
	case "true":
		*b = true
	case "false":
		*b = false
	default:
		return errors.New("expected a boolean")
	}
	return nil
}

func (tb *Toolbox) searchCode(args searchCodeArgs) (*Call, error) {
	if err := required("pattern", args.Pattern); err != nil {
		return nil, err
	}
	if _, err := path.Match(args.FileGlob, ""); err != nil {
		return nil, fmt.Errorf("invalid file_glob: %v", err)
	}
	var re *regexp.Regexp
	if args.Regex || args.IgnoreCase {
		pattern := args.Pattern
		if !args.Regex {
			pattern = regexp.QuoteMeta(pattern)
		}
		if args.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		var err error
		re, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %v", err)
		}
	}
	t, err := tb.roots.resolve(cmp.Or(args.Path, "."))
	if err != nil {
		return nil, err
	}
	return &Call{
		Title:     fmt.Sprintf("Search for %q", args.Pattern),
		Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			s := searcher{pattern: []byte(args.Pattern), re: re, glob: args.FileGlob, show: func(rel string) string {
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
	re      *regexp.Regexp
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
		if !s.matchLine(lines.Bytes()) {
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

func (s *searcher) matchLine(line []byte) bool {
	if s.re != nil {
		return s.re.Match(line)
	}
	return bytes.Contains(line, s.pattern)
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
