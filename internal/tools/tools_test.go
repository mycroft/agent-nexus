package tools

import (
	"strings"
	"testing"
)

func TestNumberLines(t *testing.T) {
	content := "a\nb\nc\nd\n"
	if got := numberLines(content, 0, 0); got != "    1  a\n    2  b\n    3  c\n    4  d\n" {
		t.Errorf("whole file = %q", got)
	}
	if got := numberLines(content, 2, 3); got != "    2  b\n    3  c\n(showing lines 2-3 of 4)\n" {
		t.Errorf("range = %q", got)
	}
	if got := numberLines(content, 9, 0); got != "(file has 4 lines)" {
		t.Errorf("past end = %q", got)
	}
	if got := numberLines(content, 3, 99); !strings.HasSuffix(got, "(showing lines 3-4 of 4)\n") {
		t.Errorf("clamped end = %q", got)
	}
}

func TestLimitLines(t *testing.T) {
	if got := limitLines("1\n2\n3\n", 2); got != "1\n2\n(2 more matches omitted; narrow the pattern or path)\n" {
		t.Errorf("limitLines = %q", got)
	}
}
