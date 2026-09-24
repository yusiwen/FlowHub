package provision

import (
	"fmt"
	"strings"
)

// diffLineLimit caps the work a diff does. The artifacts are a few kilobytes, so
// this only ever triggers on a file an operator has replaced with something large
// — and then a summary is more useful than a page of diff.
const diffLineLimit = 600

// maxDiffLines bounds the rendered output for the same reason.
const maxDiffLines = 200

// unifiedDiff renders a line diff between what FlowHub would write and what is on
// disk, so a refusal can show the operator exactly what they changed.
//
// It is deliberately small: no external diff binary (the whole point of this
// package is to not depend on the host having anything beyond git), no timestamps,
// and a hard cap on the output.
func unifiedDiff(oldText, newText string, context int) string {
	if oldText == newText {
		return ""
	}
	if context < 0 {
		context = 3
	}

	oldLines := splitLines(oldText)
	newLines := splitLines(newText)
	if len(oldLines) > diffLineLimit || len(newLines) > diffLineLimit {
		return fmt.Sprintf("(files are too different in size to diff here: %d lines vs %d lines)\n",
			len(oldLines), len(newLines))
	}

	script := diffScript(oldLines, newLines)
	hunks := buildHunks(script, context)

	var out strings.Builder
	fmt.Fprintf(&out, "--- what is on disk\n+++ what this build would write\n")
	lines := 0
	for _, hunk := range hunks {
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", hunk.oldStart+1, hunk.oldCount, hunk.newStart+1, hunk.newCount)
		for _, line := range hunk.lines {
			if lines >= maxDiffLines {
				fmt.Fprintf(&out, "... diff truncated at %d lines\n", maxDiffLines)
				return out.String()
			}
			out.WriteString(line)
			out.WriteByte('\n')
			lines++
		}
	}
	return out.String()
}

func splitLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// diffScript is a plain longest-common-subsequence diff. Quadratic, which is why
// diffLineLimit exists.
func diffScript(oldLines, newLines []string) []diffOp {
	n, m := len(oldLines), len(newLines)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1
				continue
			}
			table[i][j] = max(table[i+1][j], table[i][j+1])
		}
	}

	var ops []diffOp
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && oldLines[i] == newLines[j]:
			ops = append(ops, diffOp{' ', oldLines[i]})
			i, j = i+1, j+1
		// Removals come before additions on a tie, which is the order every diff
		// reader expects for a changed line; emitting '+' first reads as if the
		// replacement were an insertion followed by an unrelated deletion.
		case i < n && (j == m || table[i+1][j] >= table[i][j+1]):
			ops = append(ops, diffOp{'-', oldLines[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', newLines[j]})
			j++
		}
	}
	return ops
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []string
}

// buildHunks groups the edit script into hunks with a few unchanged lines of
// context, so the output shows where a change is rather than only what changed.
func buildHunks(ops []diffOp, context int) []hunk {
	var hunks []hunk
	oldLine, newLine := 0, 0

	for index := 0; index < len(ops); {
		if ops[index].kind == ' ' {
			oldLine++
			newLine++
			index++
			continue
		}
		// Start a hunk a little before the first change.
		start := index - context
		if start < 0 {
			start = 0
		}
		// Rewind the counters to the hunk's first line.
		oldStart, newStart := oldLine, newLine
		for back := index - 1; back >= start; back-- {
			if ops[back].kind != '+' {
				oldStart--
			}
			if ops[back].kind != '-' {
				newStart--
			}
		}

		current := hunk{oldStart: oldStart, newStart: newStart}
		for cursor := start; cursor < len(ops); cursor++ {
			if cursor > index && ops[cursor].kind == ' ' && trailingEqual(ops, cursor) > context {
				break
			}
			current.lines = append(current.lines, string(ops[cursor].kind)+ops[cursor].line)
			switch ops[cursor].kind {
			case ' ':
				current.oldCount++
				current.newCount++
				oldLine++
				newLine++
			case '-':
				current.oldCount++
				oldLine++
			case '+':
				current.newCount++
				newLine++
			}
			index = cursor + 1
		}
		hunks = append(hunks, current)
	}
	return hunks
}

// trailingEqual counts the unchanged lines that follow a position, which is how a
// hunk decides it has shown enough context.
func trailingEqual(ops []diffOp, from int) int {
	count := 0
	for index := from; index < len(ops) && ops[index].kind == ' '; index++ {
		count++
	}
	return count
}
