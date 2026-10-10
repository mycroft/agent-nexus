// Package diff parses `git diff` output into per-file patches and tracks
// which new-side lines a review comment may be anchored to.
package diff

import (
	"fmt"
	"strconv"
	"strings"
)

// Status is how a file changed.
type Status string

// File statuses.
const (
	Added    Status = "added"
	Deleted  Status = "deleted"
	Modified Status = "modified"
	Renamed  Status = "renamed"
)

// File is one file section of a diff.
type File struct {
	OldPath   string
	NewPath   string
	Status    Status
	Binary    bool
	Additions int
	Deletions int
	Hunks     []Hunk
}

// Hunk is one @@ section.
type Hunk struct {
	Header string
	Lines  []Line
}

// Line is one hunk line. Kind is '+', '-' or ' '. NewLine is the line
// number in the new file (0 for removed lines); OldLine likewise for the
// old file.
type Line struct {
	Kind    byte
	Text    string
	OldLine int
	NewLine int
}

// Path returns the path a reviewer refers to: the new path, or the old one
// for deleted files.
func (f *File) Path() string {
	if f.Status == Deleted {
		return f.OldPath
	}
	return f.NewPath
}

// Commentable reports whether line (new-side numbering) appears in the
// diff as an added or context line, which both forges accept as an inline
// comment anchor.
func (f *File) Commentable(line int) bool {
	if line <= 0 {
		return false
	}
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.NewLine == line {
				return true
			}
		}
	}
	return false
}

// Annotated renders the file's hunks with new-side line numbers in a
// left gutter, so a model can cite exact lines without counting:
//
//	@@ -10,3 +10,4 @@ func f
//	   10   context
//	   11 + added
//	      - removed
func (f *File) Annotated() string {
	var b strings.Builder
	for _, h := range f.Hunks {
		b.WriteString(h.Header)
		b.WriteByte('\n')
		for _, l := range h.Lines {
			if l.Kind == '-' {
				b.WriteString("      ")
			} else {
				fmt.Fprintf(&b, "%5d ", l.NewLine)
			}
			b.WriteByte(l.Kind)
			b.WriteByte(' ')
			b.WriteString(l.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Parse parses the output of `git diff` (unified format, no color).
func Parse(s string) ([]*File, error) {
	var (
		files   []*File
		cur     *File
		hunk    *Hunk
		oldLine int
		newLine int
	)
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			cur = &File{Status: Modified}
			cur.OldPath, cur.NewPath = splitGitHeader(strings.TrimPrefix(line, "diff --git "))
			files = append(files, cur)
			hunk = nil
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: content before the first file header", i+1)
		}
		if hunk != nil {
			switch {
			case strings.HasPrefix(line, "+"):
				hunk.Lines = append(hunk.Lines, Line{Kind: '+', Text: line[1:], NewLine: newLine})
				newLine++
				cur.Additions++
				continue
			case strings.HasPrefix(line, "-"):
				hunk.Lines = append(hunk.Lines, Line{Kind: '-', Text: line[1:], OldLine: oldLine})
				oldLine++
				cur.Deletions++
				continue
			case strings.HasPrefix(line, " "), line == "":
				text := ""
				if line != "" {
					text = line[1:]
				}
				hunk.Lines = append(hunk.Lines, Line{Kind: ' ', Text: text, OldLine: oldLine, NewLine: newLine})
				oldLine++
				newLine++
				continue
			case strings.HasPrefix(line, `\`):
				continue // "\ No newline at end of file"
			}
		}
		switch {
		case strings.HasPrefix(line, "@@ "):
			o, n, err := parseHunkHeader(line)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			cur.Hunks = append(cur.Hunks, Hunk{Header: line})
			hunk = &cur.Hunks[len(cur.Hunks)-1]
			oldLine, newLine = o, n
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = Added
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = Deleted
		case strings.HasPrefix(line, "rename from "):
			cur.Status = Renamed
			cur.OldPath = unquote(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			cur.Status = Renamed
			cur.NewPath = unquote(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "--- "):
			if p := strings.TrimPrefix(line, "--- "); p != "/dev/null" {
				cur.OldPath = stripPrefix(unquote(p), "a/")
			}
		case strings.HasPrefix(line, "+++ "):
			if p := strings.TrimPrefix(line, "+++ "); p != "/dev/null" {
				cur.NewPath = stripPrefix(unquote(p), "b/")
			}
		case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			cur.Binary = true
		}
	}
	return files, nil
}

// parseHunkHeader returns the starting old and new line numbers of
// "@@ -a,b +c,d @@ ...".
func parseHunkHeader(line string) (int, int, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	o, err := rangeStart(fields[1][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: %w", line, err)
	}
	n, err := rangeStart(fields[2][1:])
	if err != nil {
		return 0, 0, fmt.Errorf("hunk header %q: %w", line, err)
	}
	return o, n, nil
}

func rangeStart(r string) (int, error) {
	start, _, _ := strings.Cut(r, ",")
	return strconv.Atoi(start)
}

// splitGitHeader splits "a/x b/y" (possibly quoted). Paths with spaces are
// ambiguous here; the ---/+++ and rename lines that follow override them
// whenever the file has content.
func splitGitHeader(h string) (string, string) {
	if strings.HasPrefix(h, `"`) {
		if a, rest, ok := cutQuoted(h); ok {
			return stripPrefix(a, "a/"), stripPrefix(unquote(strings.TrimSpace(rest)), "b/")
		}
	}
	if i := strings.LastIndex(h, ` "b/`); i >= 0 {
		return stripPrefix(h[:i], "a/"), stripPrefix(unquote(h[i+1:]), "b/")
	}
	// Unquoted: for an unrenamed file both halves are equal, so split in
	// the middle when that holds.
	if len(h)%2 == 1 {
		mid := len(h) / 2
		a, b := h[:mid], h[mid+1:]
		if h[mid] == ' ' && stripPrefix(a, "a/") == stripPrefix(b, "b/") {
			return stripPrefix(a, "a/"), stripPrefix(b, "b/")
		}
	}
	if i := strings.Index(h, " b/"); i >= 0 {
		return stripPrefix(h[:i], "a/"), stripPrefix(h[i+1:], "b/")
	}
	return h, h
}

func cutQuoted(s string) (string, string, bool) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			q, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", false
			}
			return q, s[i+1:], true
		}
	}
	return "", "", false
}

func unquote(p string) string {
	if strings.HasPrefix(p, `"`) {
		if q, err := strconv.Unquote(p); err == nil {
			return q
		}
	}
	return p
}

func stripPrefix(p, prefix string) string {
	return strings.TrimPrefix(p, prefix)
}
