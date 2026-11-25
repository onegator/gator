package marketplace

import (
	"fmt"
	"strings"
)

// maxDiffCells bounds the comparison table (lines of one text times lines of the other, a few
// megabytes at most); past it, a diff says only that the texts differ.
const maxDiffCells = 250_000

// Diff is a unified diff of two texts, line by line: what a person changed of an installed agent's
// instructions, or what a new version would change. Empty when they are the same.
func Diff(from, to, fromName, toName string) string {
	if from == to {
		return ""
	}
	a, b := lines(from), lines(to)
	if (len(a)+1)*(len(b)+1) > maxDiffCells {
		return fmt.Sprintf("--- %s\n+++ %s\n(the texts differ; they are too long to compare line by line)\n", fromName, toName)
	}
	// The longest common subsequence, then a walk that writes it out.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", fromName, toName)
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			out.WriteString(" " + a[i] + "\n")
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			out.WriteString("-" + a[i] + "\n")
			i++
		default:
			out.WriteString("+" + b[j] + "\n")
			j++
		}
	}
	return out.String()
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
