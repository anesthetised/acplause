package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

var getInstructionsSpec = Spec{
	Name:        "get_instructions",
	Kind:        acp.ToolRead,
	Description: "Read source-labelled AGENTS.md instructions from the workspace root to a directory. Before editing a file, look up its containing directory. Later sources take precedence only within their subtree; tool policy still applies.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Existing directory, relative to cwd or absolute within a workspace root"}
		},
		"required": ["path"]
	}`),
}

type getInstructionsArgs struct {
	Path string `json:"path"`
}

func (tb *Toolbox) getInstructions(args getInstructionsArgs) (*Call, error) {
	if err := required("path", args.Path); err != nil {
		return nil, err
	}
	t, err := tb.roots.resolve(args.Path)
	if err != nil {
		return nil, err
	}
	return &Call{
		Title: "Instructions for " + args.Path, Locations: []string{t.abs},
		run: func(ctx context.Context) (string, error) {
			chain, err := tb.ResolveInstructions(ctx, args.Path)
			if err != nil {
				return "", err
			}
			var out strings.Builder
			out.WriteString("Project instructions in root-to-directory order. Later sources take precedence only within their directory's subtree. Enforced mode, sandbox and permission policy always apply.\n")
			if len(chain) == 0 {
				out.WriteString("No AGENTS.md files found along this directory chain.\n")
			}
			for _, source := range chain {
				fmt.Fprintf(&out, "\nSource: %s\n%s\n", source.Path, source.Text)
			}
			return out.String(), nil
		},
	}, nil
}

const (
	maxInstructionDirs  = 32
	maxInstructionBytes = 128 << 10
)

// Instruction retains its source so callers can distinguish rules for different
// subtrees instead of treating all project prose as global policy.
type Instruction struct {
	// Path is absolute to keep sources unambiguous across workspace roots.
	Path string
	// Text preserves the file's validated UTF-8 bytes, including whitespace.
	Text string
}

// ResolveInstructions reads the root-to-directory instruction chain within the
// workspace root selected by normal path resolution. Later sources are more
// specific and apply only to their subtree. Each call rereads live files and
// returns no partial chain on a limit, cancellation or file error.
func (tb *Toolbox) ResolveInstructions(ctx context.Context, directory string) ([]Instruction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, err := tb.roots.resolve(directory)
	if err != nil {
		return nil, err
	}
	dirs := []string{t.rel}
	for dirs[len(dirs)-1] != "." {
		if len(dirs) == maxInstructionDirs {
			return nil, fmt.Errorf("%s exceeds instruction lookup limit of %d directories", t.abs, maxInstructionDirs)
		}
		dirs = append(dirs, filepath.Dir(dirs[len(dirs)-1]))
	}
	info, err := t.root.Stat(t.rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", t.abs)
	}
	var chain []Instruction
	total := 0
	for _, dir := range slices.Backward(dirs) {
		source, err := readInstruction(ctx, tb.roots.within(t.root, filepath.Join(dir, "AGENTS.md")))
		if err != nil {
			return nil, err
		}
		if source == nil {
			continue
		}
		total += len(source.Text)
		if total > maxInstructionBytes {
			return nil, fmt.Errorf("instruction text at %s exceeds aggregate limit of %d bytes", source.Path, maxInstructionBytes)
		}
		chain = append(chain, *source)
	}
	return chain, nil
}

// readInstruction distinguishes an absent file from a present but unreadable
// one, including a dangling symlink. The descriptor-based reader still decides
// regular-file safety and size after opening; metadata alone is not trusted.
func readInstruction(ctx context.Context, t target) (*Instruction, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", t.abs, err)
	}
	if _, err := t.root.Lstat(t.rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: %w", t.abs, err)
	}
	data, err := t.readFile(ctx, maxInstructionsSize)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.abs, err)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", t.abs)
	}
	abs, err := filepath.Abs(t.abs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.abs, err)
	}
	return &Instruction{Path: abs, Text: string(data)}, nil
}
