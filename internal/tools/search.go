package tools

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
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
	maxMatches       = 50
	maxMatchWidth    = 300
	maxSearchContext = 5
)

var searchCodeSpec = Spec{
	Name:        "search_code",
	Kind:        acp.ToolSearch,
	Description: fmt.Sprintf("Search text files for a pattern, literal and case-sensitive by default. Supports regex, case folding, matching-line offset/limit and bounded context. Returns up to %d matching lines as path:line:text; context uses path-line-text. Explicit paging/context reports continuation. Each call traverses files afresh in lexical order.", maxMatches),
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"pattern": {"type": "string", "description": "Text to search for, or a Go regular expression when regex is true"},
			"regex": {"type": "boolean", "description": "Treat pattern as a Go regular expression, matched per line (default false)"},
			"ignore_case": {"type": "boolean", "description": "Use Unicode case-insensitive matching (default false)"},
			"offset": {"type": "integer", "minimum": 0, "description": "Skip this many matching lines, not context lines (default 0)"},
			"limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum matching lines in the page (default 50)"},
			"context_lines": {"type": "integer", "minimum": 0, "maximum": 5, "description": "Lines before and after each page match, with overlapping windows merged (default 0)"},
			"include_hidden": {"type": "boolean", "description": "Include hidden descendants except .git (default false); an explicit hidden starting path is allowed, and hidden configuration may reach the model"},
			"file_glob": {"type": "string", "description": "Only search files whose name matches this glob (e.g. '*.go')"},
			"path": {"type": "string", "description": "Directory to search, relative to the working directory or absolute (default '.')"}
		},
		"required": ["pattern"]
	}`),
}

type searchCodeArgs struct {
	Pattern       string       `json:"pattern"`
	Regex         optionalBool `json:"regex"`
	IgnoreCase    optionalBool `json:"ignore_case"`
	FileGlob      string       `json:"file_glob"`
	Path          string       `json:"path"`
	Offset        optionalInt  `json:"offset"`
	Limit         optionalInt  `json:"limit"`
	ContextLines  optionalInt  `json:"context_lines"`
	IncludeHidden optionalBool `json:"include_hidden"`
}

// optionalInt distinguishes an omitted option from an explicit zero and rejects
// null instead of treating it as the default.
type optionalInt struct {
	value int
	set   bool
}

// UnmarshalJSON accepts an explicit integer and records its presence.
func (n *optionalInt) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		return errors.New("expected an integer")
	}
	if err := json.Unmarshal(raw, &n.value); err != nil {
		return err
	}
	n.set = true
	return nil
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
	paged := args.Offset.set || args.Limit.set || args.ContextLines.set
	if !args.Limit.set {
		args.Limit.value = maxMatches
	}
	if args.Offset.value < 0 {
		return nil, errors.New("offset must be at least 0")
	}
	if args.Limit.value < 1 || args.Limit.value > maxMatches {
		return nil, fmt.Errorf("limit must be between 1 and %d", maxMatches)
	}
	if args.ContextLines.value < 0 || args.ContextLines.value > maxSearchContext {
		return nil, fmt.Errorf("context_lines must be between 0 and %d", maxSearchContext)
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
	if err := checkTraversalPath(t.abs); err != nil {
		return nil, err
	}
	return &Call{
		Title:     fmt.Sprintf("Search for %q", args.Pattern),
		Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			s := searcher{pattern: []byte(args.Pattern), re: re, glob: args.FileGlob,
				offset: args.Offset.value, limit: args.Limit.value, contextLines: args.ContextLines.value,
				start: filepath.ToSlash(t.rel), includeHidden: bool(args.IncludeHidden),
				show: func(rel string) string {
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
				if paged {
					fmt.Fprintf(&s.out, "... more matches; next offset: %d\n", s.skipped+s.matches)
				} else {
					fmt.Fprintf(&s.out, "... stopped after %d matches\n", maxMatches)
				}
				err = nil
			} else if err == nil && paged && s.matches > 0 {
				s.out.WriteString("... no more matches\n")
			}
			if err == nil && s.matches == 0 {
				if paged {
					return fmt.Sprintf("No more matches for %q at offset %d", args.Pattern, args.Offset.value), nil
				}
				return fmt.Sprintf("No matches for %q", args.Pattern), nil
			}
			return s.out.String(), err
		},
	}, nil
}

var errSearchFull = errors.New("enough matches")

type searcher struct {
	pattern       []byte
	re            *regexp.Regexp
	glob          string
	show          func(rel string) string
	out           strings.Builder
	matches       int
	offset        int
	limit         int
	contextLines  int
	skipped       int
	start         string
	includeHidden bool
}

func (s *searcher) visit(ctx context.Context, t target, d fs.DirEntry) error {
	name := filepath.ToSlash(t.rel)
	// Only the explicitly selected start bypasses the hidden-name default;
	// generated names and .git still use the shared exclusion predicate.
	if name != "." && ignored(d.Name(), s.includeHidden || name == s.start) {
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
	return s.scan(ctx, data, name)
}

// scan retains only a small preceding window. Trailing context is emitted before
// stopping at the first extra match, even when that match is inside the window.
func (s *searcher) scan(ctx context.Context, data []byte, name string) error {
	lines := bufio.NewScanner(bytes.NewReader(data))
	lines.Buffer(nil, maxReadSize+1)
	var before [maxSearchContext]searchLine
	count, lastPrinted, tailUntil := 0, 0, 0
	more := false
	limit := cmp.Or(s.limit, maxMatches)
	emit := func(line searchLine, separator string) {
		fmt.Fprintf(&s.out, "%s%s%d%s%s\n", s.show(name), separator, line.number, separator, line.text)
		lastPrinted = line.number
	}
	for n := 1; lines.Scan(); n++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		selected := false
		if !more && s.matchLine(lines.Bytes()) {
			switch {
			case s.skipped < s.offset:
				s.skipped++
			case s.matches < limit:
				s.matches++
				selected = true
			default:
				more = true
			}
		}
		if selected || n <= tailUntil || (s.contextLines > 0 && s.matches < limit) {
			line := searchLine{n, clip(string(bytes.TrimSpace(lines.Bytes())), maxMatchWidth)}
			if selected {
				for _, previous := range before[:count] {
					if previous.number > lastPrinted {
						emit(previous, "-")
					}
				}
				emit(line, ":")
				tailUntil = n + s.contextLines
			} else if n <= tailUntil {
				emit(line, "-")
			}
			if s.contextLines > 0 && s.matches < limit {
				if count == s.contextLines {
					copy(before[:], before[1:count])
					count--
				}
				before[count] = line
				count++
			}
		}
		if more && n >= tailUntil {
			return errSearchFull
		}
	}
	if more {
		return errSearchFull
	}
	return lines.Err()
}

type searchLine struct {
	number int
	text   string
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
