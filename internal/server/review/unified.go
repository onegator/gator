package review

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
)

// fileDiff is one file's part of a unified diff: its header lines and its hunks, as text.
type fileDiff struct {
	path   string
	header []string
	hunks  [][]string
}

// parseUnified splits a unified diff (git diff) into its files. Lines before the first file are
// dropped; a file's path is the one its "diff --git" line names for the new side.
func parseUnified(diff []byte) []fileDiff {
	var out []fileDiff
	var cur *fileDiff
	for line := range strings.SplitSeq(strings.TrimRight(string(diff), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			out = append(out, fileDiff{path: diffPath(line), header: []string{line}})
			cur = &out[len(out)-1]
		case cur == nil:
		case strings.HasPrefix(line, "@@"):
			cur.hunks = append(cur.hunks, []string{line})
		case len(cur.hunks) == 0:
			cur.header = append(cur.header, line)
		default:
			cur.hunks[len(cur.hunks)-1] = append(cur.hunks[len(cur.hunks)-1], line)
		}
	}
	return out
}

// diffPath is the new side's path of a "diff --git a/x b/x" line.
func diffPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}

func (f fileDiff) write(b *bytes.Buffer) {
	for _, l := range f.header {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	for _, h := range f.hunks {
		for _, l := range h {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
}

// IgnoreWhitespace takes out of a unified diff the changes that only change whitespace: a run of
// removed lines followed by as many added lines, each the same as its pair but for whitespace,
// becomes context (the new lines), so the hunk headers still count right. A hunk with no change
// left goes, and so does a file with no hunk left that had hunks (a binary or a mode change has
// none, and stays). It is close to git's -w, worked out from the diff alone: the server keeps the
// diffs, not the repository.
func IgnoreWhitespace(diff []byte) []byte {
	var b bytes.Buffer
	for _, f := range parseUnified(diff) {
		had := len(f.hunks) > 0
		var kept [][]string
		for _, h := range f.hunks {
			if h2, changed := ignoreWhitespaceHunk(h); changed {
				kept = append(kept, h2)
			}
		}
		if had && len(kept) == 0 {
			continue
		}
		f.hunks = kept
		f.write(&b)
	}
	return b.Bytes()
}

// ignoreWhitespaceHunk turns whitespace-only pairs into context, and says whether a change is left.
func ignoreWhitespaceHunk(h []string) ([]string, bool) {
	out := []string{h[0]}
	changed := false
	for i := 1; i < len(h); {
		if !strings.HasPrefix(h[i], "-") {
			if strings.HasPrefix(h[i], "+") {
				changed = true
			}
			out = append(out, h[i])
			i++
			continue
		}
		j := i
		for j < len(h) && strings.HasPrefix(h[j], "-") {
			j++
		}
		k := j
		for k < len(h) && strings.HasPrefix(h[k], "+") {
			k++
		}
		removed, added := h[i:j], h[j:k]
		if len(removed) == len(added) && sameButWhitespace(removed, added) {
			for _, l := range added {
				out = append(out, " "+l[1:])
			}
		} else {
			out = append(out, h[i:k]...)
			changed = true
		}
		i = k
	}
	return out, changed
}

func sameButWhitespace(removed, added []string) bool {
	for n := range removed {
		if squeeze(removed[n][1:]) != squeeze(added[n][1:]) {
			return false
		}
	}
	return true
}

func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// Compose puts the diffs of several turns together, file by file in the order the files first
// appear: each file's parts follow one another, turn after turn. It is not one diff recomputed
// from the first turn's base (the server has no repository); for the whole job from its first
// turn, the last checkpoint's diff of the whole job is that, exactly.
func Compose(diffs [][]byte) []byte {
	var order []string
	byPath := map[string][]fileDiff{}
	for _, d := range diffs {
		for _, f := range parseUnified(d) {
			if _, seen := byPath[f.path]; !seen {
				order = append(order, f.path)
			}
			byPath[f.path] = append(byPath[f.path], f)
		}
	}
	var b bytes.Buffer
	for _, p := range order {
		for _, f := range byPath[p] {
			f.write(&b)
		}
	}
	return b.Bytes()
}

// maxSnippet bounds the code a comment carries to the agent.
const maxSnippet = 4000

// Snippet is the code a comment is about: the lines start..end of path on one side of a diff
// ("old" numbers the file before, "new" after), each with its diff marker, as the diff shows them.
// It answers false when the diff does not show those lines.
func Snippet(diff []byte, path, side string, start, end int) (string, bool) {
	for _, f := range parseUnified(diff) {
		if f.path != path {
			continue
		}
		var b strings.Builder
		found := false
		for _, h := range f.hunks {
			oldLine, newLine, ok := hunkStart(h[0])
			if !ok {
				continue
			}
			for _, l := range h[1:] {
				if l == "" || l[0] == '\\' {
					continue
				}
				n, counts := 0, false
				switch l[0] {
				case '-':
					n, counts = oldLine, side == "old"
					oldLine++
				case '+':
					n, counts = newLine, side == "new"
					newLine++
				default:
					n, counts = newLine, true
					if side == "old" {
						n = oldLine
					}
					oldLine++
					newLine++
				}
				if counts && n >= start && n <= end {
					found = true
					if b.Len()+len(l) < maxSnippet {
						b.WriteString(l)
						b.WriteByte('\n')
					}
				}
			}
		}
		if found {
			return b.String(), true
		}
	}
	return "", false
}

// hunkStart reads the first old and new line numbers of a hunk header, "@@ -a,b +c,d @@".
func hunkStart(header string) (oldLine, newLine int, ok bool) {
	var a, c int
	rest := strings.TrimPrefix(header, "@@ ")
	fields := strings.Fields(rest)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "-") || !strings.HasPrefix(fields[1], "+") {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.SplitN(fields[0][1:], ",", 2)[0], "%d", &a); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(strings.SplitN(fields[1][1:], ",", 2)[0], "%d", &c); err != nil {
		return 0, 0, false
	}
	return a, c, true
}
