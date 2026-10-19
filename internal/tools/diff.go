package tools

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/anesthetised/acplause/internal/acp"
)

const (
	diffContext = 3 // unchanged lines around a change, as in git
	// maxDiffCells bounds the LCS table (about 2 bytes per cell). It applies
	// to the lines left after trimming the common head and tail, so a small
	// edit in a large file still gets a patch. Beyond it the change is sent
	// without a patch.
	maxDiffCells = 4_000_000
)

// fileDiff describes replacing the file at abs, which did not exist when
// !existed, with new. The changes stay authoritative; the patch is nil when
// there is nothing to render (identical content, a new empty file, whose
// header-only patch with absolute paths git itself cannot parse), when
// JSON would mangle non-UTF-8 text, or when the inputs are too large.
func fileDiff(abs, old string, existed bool, new string) acp.ToolCallContent {
	op := acp.DiffModify
	if !existed {
		op = acp.DiffAdd
	}
	changes := []acp.DiffChange{{Operation: op, Path: abs, FileType: acp.FileText}}
	if old == new || !utf8.ValidString(old) || !utf8.ValidString(new) {
		return acp.ToolDiff(changes, nil)
	}
	ops, ok := diffLines(splitLines(old), splitLines(new))
	if !ok {
		return acp.ToolDiff(changes, nil)
	}
	var b strings.Builder
	p := quotePath(abs)
	fmt.Fprintf(&b, "diff --git %s %s\n", p, p)
	from := p
	if !existed {
		b.WriteString("new file mode 100644\n")
		from = "/dev/null"
	}
	fmt.Fprintf(&b, "--- %s%s\n+++ %s%s\n", from, spaceTab(from), p, spaceTab(p))
	writeHunks(&b, ops)
	return acp.ToolDiff(changes, &acp.DiffPatch{Format: acp.PatchGit, Text: b.String()})
}

// spaceTab is the tab git appends to unquoted names with spaces in ---/+++
// lines, so patch parsers can tell where the name ends.
func spaceTab(name string) string {
	if strings.Contains(name, " ") && !strings.HasPrefix(name, `"`) {
		return "\t"
	}
	return ""
}

var cEscapes = map[byte]string{7: `\a`, 8: `\b`, 9: `\t`, 10: `\n`, 11: `\v`, 12: `\f`, 13: `\r`}

// quotePath quotes a path the way git does with core.quotePath on: C-style
// escapes, octal for other control and non-ASCII bytes.
func quotePath(p string) string {
	plain := true
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < ' ' || c == '"' || c == '\\' || c >= 0x7f {
			plain = false
		}
	}
	if plain {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c >= ' ' && c < 0x7f:
			b.WriteByte(c)
		default:
			if e, ok := cEscapes[c]; ok {
				b.WriteString(e)
			} else {
				fmt.Fprintf(&b, `\%03o`, c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// splitLines keeps line terminators, so a missing final newline makes the
// last line differ from its terminated twin.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp struct {
	kind byte // ' ', '-' or '+'
	line string
}

// diffLines returns the edit script from a to b, or false when the inputs
// are too large. It trims the common head and tail, then runs an LCS.
func diffLines(a, b []string) ([]diffOp, bool) {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	ma, mb := a[head:len(a)-tail], b[head:len(b)-tail]
	if (len(ma)+1)*(len(mb)+1) > maxDiffCells {
		return nil, false
	}
	// lcs[i][j] is the LCS length of ma[i:] and mb[j:].
	w := len(mb) + 1
	lcs := make([]uint16, (len(ma)+1)*w)
	for i := len(ma) - 1; i >= 0; i-- {
		for j := len(mb) - 1; j >= 0; j-- {
			if ma[i] == mb[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}
	ops := make([]diffOp, 0, len(a)+len(mb))
	for _, l := range a[:head] {
		ops = append(ops, diffOp{' ', l})
	}
	i, j := 0, 0
	for i < len(ma) || j < len(mb) {
		switch {
		case i < len(ma) && j < len(mb) && ma[i] == mb[j]:
			ops = append(ops, diffOp{' ', ma[i]})
			i++
			j++
		case i < len(ma) && (j == len(mb) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
			ops = append(ops, diffOp{'-', ma[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', mb[j]})
			j++
		}
	}
	for _, l := range a[len(a)-tail:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops, true
}

// writeHunks renders ops as unified hunks, merging changes whose context
// overlaps or touches, like git.
func writeHunks(b *strings.Builder, ops []diffOp) {
	oldN, newN := 0, 0 // lines consumed before ops[pos]
	pos := 0
	advance := func(to int) {
		for ; pos < to; pos++ {
			if ops[pos].kind != '+' {
				oldN++
			}
			if ops[pos].kind != '-' {
				newN++
			}
		}
	}
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		last := i // last changed op of this hunk
		for j := i; j < len(ops) && j-last <= 2*diffContext+1; j++ {
			if ops[j].kind != ' ' {
				last = j
			}
		}
		start, end := max(i-diffContext, pos), min(last+diffContext+1, len(ops))
		advance(start)
		o, n := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				o++
			}
			if op.kind != '-' {
				n++
			}
		}
		fmt.Fprintf(b, "@@ -%s +%s @@\n", hunkRange(oldN, o), hunkRange(newN, n))
		for _, op := range ops[start:end] {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
			if !strings.HasSuffix(op.line, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
		advance(end)
		i = end
	}
}

// hunkRange formats a start line and count; an empty range starts at the
// line before it.
func hunkRange(before, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", before)
	case 1:
		return strconv.Itoa(before + 1)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}
