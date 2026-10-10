// Package skill loads Agent Skills (SKILL.md with name/description
// frontmatter) to use as agent instructions.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Skill is a loaded SKILL.md.
type Skill struct {
	Name        string
	Description string
	// Body is the markdown after the frontmatter.
	Body string
}

// Load reads <name>/SKILL.md from fsys.
func Load(fsys fs.FS, name string) (Skill, error) {
	raw, err := fs.ReadFile(fsys, name+"/SKILL.md")
	if err != nil {
		return Skill{}, fmt.Errorf("load skill %q: %w", name, err)
	}
	s, err := Parse(string(raw))
	if err != nil {
		return Skill{}, fmt.Errorf("load skill %q: %w", name, err)
	}
	if s.Name != name {
		return Skill{}, fmt.Errorf("load skill %q: frontmatter name is %q", name, s.Name)
	}
	return s, nil
}

// Parse parses a SKILL.md document. Only single-line "key: value"
// frontmatter entries are supported, which is all the skills here use.
func Parse(doc string) (Skill, error) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	rest, ok := strings.CutPrefix(doc, "---\n")
	if !ok {
		return Skill{}, errors.New("missing frontmatter")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Skill{}, errors.New("unterminated frontmatter")
	}
	var s Skill
	for line := range strings.SplitSeq(front, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			s.Name = value
		case "description":
			s.Description = value
		}
	}
	if s.Name == "" || s.Description == "" {
		return Skill{}, errors.New("frontmatter needs name and description")
	}
	s.Body = strings.TrimSpace(body)
	return s, nil
}
