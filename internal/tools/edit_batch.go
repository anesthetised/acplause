package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

var editBatchSpec = Spec{
	Name:        "edit_batch",
	Kind:        acp.ToolEdit,
	Description: "Apply 1..50 exact replacements in order to one existing UTF-8 file, with one approved diff. Each old_text must occur exactly once at its step. Input and final output are capped at 5 MiB. A changed file requires re-reading and new approval. Unavailable in Plan mode.",
	Parameters: []byte(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Existing file, relative to cwd or absolute within a workspace root"},
			"edits": {
				"type": "array", "minItems": 1, "maxItems": 50,
				"items": {
					"type": "object",
					"properties": {
						"old_text": {"type": "string", "minLength": 1, "description": "Exact text occurring once in the candidate at this step"},
						"new_text": {"type": "string", "description": "Replacement text; an explicit empty string deletes the match"}
					},
					"required": ["old_text", "new_text"]
				}
			}
		},
		"required": ["path", "edits"]
	}`),
}

type batchEdit struct {
	OldText *string `json:"old_text"`
	NewText *string `json:"new_text"`
}

type editBatchArgs struct {
	Path  string      `json:"path"`
	Edits []batchEdit `json:"edits"`
}

// batchPreview binds approval to bytes, not to replacements that could be
// recomputed against a different file while permission was pending.
type batchPreview struct {
	original  []byte
	candidate string
	diff      acp.ToolCallContent
}

func (tb *Toolbox) editBatch(args editBatchArgs) (*Call, error) {
	t, err := tb.roots.resolve(args.Path)
	if err != nil {
		return nil, err
	}
	if len(args.Edits) < 1 || len(args.Edits) > 50 {
		return nil, errors.New("edits must contain 1..50 replacements")
	}
	for i, edit := range args.Edits {
		if edit.OldText == nil || *edit.OldText == "" || edit.NewText == nil {
			return nil, fmt.Errorf("edit %d requires nonempty old_text and explicit string new_text", i+1)
		}
	}
	var preview *batchPreview
	c := &Call{Title: "Batch edit " + args.Path, Locations: []string{t.abs}}
	c.preview = func(ctx context.Context) (*acp.ToolCallContent, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if preview == nil {
			original, err := t.readFile(ctx, maxWriteSize)
			if err != nil {
				return nil, err
			}
			if !utf8.Valid(original) {
				return nil, fmt.Errorf("%s is not valid UTF-8", args.Path)
			}
			candidate, err := batchCandidate(ctx, string(original), args.Edits)
			if err != nil {
				return nil, err
			}
			diff := fileDiff(t.abs, string(original), true, candidate)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			preview = &batchPreview{original: original, candidate: candidate, diff: diff}
		}
		return &preview.diff, nil
	}
	c.run = func(ctx context.Context) (string, error) {
		approved := preview
		preview = nil // every execution attempt consumes this approval snapshot
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if approved == nil {
			return "", errors.New("edit_batch requires a successful preview; re-read the file and retry with new approval")
		}
		current, err := t.readFile(ctx, maxWriteSize)
		if err != nil {
			return "", fmt.Errorf("cannot verify %s; re-read the file and retry with new approval: %w", args.Path, err)
		}
		if !bytes.Equal(current, approved.original) {
			return "", fmt.Errorf("%s changed since preview; re-read the file and retry with new approval", args.Path)
		}
		// Existing descriptor checks also guard the write. This comparison is
		// not a lock against external writes after it, or crash-atomic storage.
		if err := t.writeFile(ctx, []byte(approved.candidate)); err != nil {
			return "", err
		}
		c.diff = &approved.diff
		return "Batch edited " + t.abs, nil
	}
	return c, nil
}

// batchCandidate validates all replacements before any file can be written.
// Only input and final file sizes are capped; temporary growth may be undone
// by a later edit. The edit count bounds the number of replacement passes.
func batchCandidate(ctx context.Context, content string, edits []batchEdit) (string, error) {
	for i, edit := range edits {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		first := strings.Index(content, *edit.OldText)
		if first < 0 || first != strings.LastIndex(content, *edit.OldText) {
			return "", fmt.Errorf("edit %d: old_text must occur exactly once in the candidate; re-read the file and provide a unique exact match", i+1)
		}
		content = strings.Replace(content, *edit.OldText, *edit.NewText, 1)
	}
	if len(content) > maxWriteSize {
		return "", fmt.Errorf("final content is too large (max %d bytes)", maxWriteSize)
	}
	return content, ctx.Err()
}
