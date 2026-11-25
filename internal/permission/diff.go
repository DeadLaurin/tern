package permission

import (
	"fmt"
	"strings"
)

// unifiedContext is the number of unchanged lines shown around each change.
const unifiedContext = 3

// maxDiffCells bounds the LCS table size. Beyond it the differing region is
// rendered as a full replacement, which keeps diff previews cheap for huge
// files while remaining a valid unified diff.
const maxDiffCells = 4_000_000

// UnifiedDiff renders a unified diff between oldContent and newContent for the
// given path, using only the Go standard library. It is intended for showing a
// preview of an edit before it is applied.
//
// The output uses "a/<path>" and "b/<path>" headers and hunks with three lines
// of context, e.g.:
//
//	--- a/main.go
//	+++ b/main.go
//	@@ -1,3 +1,3 @@
//	 package main
//	-var version = "1"
//	+var version = "2"
//
// An empty string is returned when the contents are identical.
func UnifiedDiff(path, oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}

	oldLines := splitDiffLines(oldContent)
	newLines := splitDiffLines(newContent)

	ops := diffOps(oldLines, newLines)

	name := strings.TrimSpace(path)
	if name == "" {
		name = "file"
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/")

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", name)
	fmt.Fprintf(&b, "+++ b/%s\n", name)

	// Assign line numbers. oldLine/newLine point at the next line to be
	// consumed in each side (1-based), matching unified diff hunk starts.
	oldLine, newLine := 1, 1
	pos := make([]diffPositioned, len(ops))
	for i, op := range ops {
		pos[i] = diffPositioned{kind: op.kind, text: op.text, oldLine: oldLine, newLine: newLine}
		switch op.kind {
		case ' ':
			oldLine++
			newLine++
		case '-':
			oldLine++
		case '+':
			newLine++
		}
	}

	for i := 0; i < len(pos); {
		// Find the next change.
		for i < len(pos) && pos[i].kind == ' ' {
			i++
		}
		if i >= len(pos) {
			break
		}
		// Extend the hunk: three context lines, merging changes that are
		// separated by no more than 2*context unchanged lines.
		start := maxInt(0, i-unifiedContext)
		end := i
		for {
			for end < len(pos) && pos[end].kind != ' ' {
				end++
			}
			// Include context after the change.
			ctx := 0
			for end < len(pos) && pos[end].kind == ' ' && ctx < unifiedContext {
				end++
				ctx++
			}
			// If another change follows within the context window, keep going.
			next := end
			for next < len(pos) && pos[next].kind == ' ' {
				next++
			}
			if next < len(pos) && next-end < unifiedContext {
				end = next
				continue
			}
			break
		}

		hunk := pos[start:end]
		oldStart, oldCount := hunkStart(hunk, 'o')
		newStart, newCount := hunkStart(hunk, 'n')
		if oldCount == 0 {
			oldStart--
		}
		if newCount == 0 {
			newStart--
		}
		if oldStart < 0 {
			oldStart = 0
		}
		if newStart < 0 {
			newStart = 0
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, p := range hunk {
			b.WriteByte(p.kind)
			b.WriteString(p.text)
			b.WriteByte('\n')
		}
		i = end
	}

	return b.String()
}

// hunkStart returns the first line number and the number of lines of the given
// side consumed by the hunk.
func hunkStart(hunk []diffPositioned, side byte) (int, int) {
	last := 0
	start := 0
	set := false
	for _, p := range hunk {
		var line int
		var counts bool
		if side == 'o' {
			line = p.oldLine
			counts = p.kind == ' ' || p.kind == '-'
		} else {
			line = p.newLine
			counts = p.kind == ' ' || p.kind == '+'
		}
		if counts {
			if !set {
				start = line
				set = true
			}
			last = line
		}
	}
	if !set {
		// Pure insertion/deletion: anchor on the position of the first op.
		if len(hunk) > 0 {
			if side == 'o' {
				start = hunk[0].oldLine
			} else {
				start = hunk[0].newLine
			}
		}
		return start, 0
	}
	return start, last - start + 1
}

// diffPositioned is a diff operation annotated with the line numbers it
// refers to on each side.
type diffPositioned struct {
	kind    byte
	text    string
	oldLine int
	newLine int
}

// splitDiffLines splits s into lines, discarding a single trailing newline.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffOp is a single edit-script operation.
type diffOp struct {
	kind byte // ' ' equal, '-' delete, '+' insert
	text string
}

// diffOps computes an edit script using LCS after trimming common prefix and
// suffix lines.
func diffOps(oldLines, newLines []string) []diffOp {
	ops := make([]diffOp, 0, len(oldLines)+len(newLines))

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	for _, line := range oldLines[:prefix] {
		ops = append(ops, diffOp{' ', line})
	}

	midOld := oldLines[prefix : len(oldLines)-suffix]
	midNew := newLines[prefix : len(newLines)-suffix]
	ops = append(ops, lcsOps(midOld, midNew)...)

	for _, line := range oldLines[len(oldLines)-suffix:] {
		ops = append(ops, diffOp{' ', line})
	}
	return ops
}

// lcsOps builds an edit script for two line slices using a longest common
// subsequence table. For very large regions it falls back to replacing the
// whole block.
func lcsOps(a, b []string) []diffOp {
	if len(a) == 0 {
		ops := make([]diffOp, len(b))
		for i, line := range b {
			ops[i] = diffOp{'+', line}
		}
		return ops
	}
	if len(b) == 0 {
		ops := make([]diffOp, len(a))
		for i, line := range a {
			ops[i] = diffOp{'-', line}
		}
		return ops
	}
	if len(a)*len(b) > maxDiffCells {
		ops := make([]diffOp, 0, len(a)+len(b))
		for _, line := range a {
			ops = append(ops, diffOp{'-', line})
		}
		for _, line := range b {
			ops = append(ops, diffOp{'+', line})
		}
		return ops
	}

	// table[i][j] = LCS length of a[i:], b[j:].
	table := make([][]int32, len(a)+1)
	for i := range table {
		table[i] = make([]int32, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	ops := make([]diffOp, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
