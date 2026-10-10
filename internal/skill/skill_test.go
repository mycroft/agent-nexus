package skill

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mycroft/agent-nexus/skills"
)

func TestParse(t *testing.T) {
	s, err := Parse("---\r\nname: demo\r\ndescription: \"Use when testing.\"\r\n---\r\n\r\n# Demo\r\nbody\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "demo" || s.Description != "Use when testing." || s.Body != "# Demo\nbody" {
		t.Errorf("Parse = %+v", s)
	}
	for _, bad := range []string{"# no frontmatter", "---\nname: x\n", "---\nname: x\n---\nbody", "---\ndescription: d\n---\nbody"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse accepted %q", bad)
		}
	}
}

func TestLoad(t *testing.T) {
	fsys := fstest.MapFS{"a/SKILL.md": {Data: []byte("---\nname: b\ndescription: d\n---\nx")}}
	if _, err := Load(fsys, "a"); err == nil {
		t.Error("Load accepted a name mismatch")
	}
	if _, err := Load(fsys, "missing"); err == nil {
		t.Error("Load accepted a missing skill")
	}
}

func TestEmbeddedPRReview(t *testing.T) {
	s, err := Load(skills.FS, "pr-review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Description, "Use when") || !strings.Contains(s.Body, "## Severity") {
		t.Errorf("unexpected pr-review skill: %q", s.Description)
	}
}
