package diff

import (
	"strings"
	"testing"
)

const sample = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,5 @@
 package main

-func main() {}
+import "fmt"
+
+func main() { fmt.Println("hi") }
@@ -10,2 +12,2 @@ func other() {
 	a := 1
-	b := 2
+	b := 3
diff --git a/old/removed.txt b/old/removed.txt
deleted file mode 100644
index 3333333..0000000
--- a/old/removed.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
diff --git a/pkg/new.go b/pkg/new.go
new file mode 100644
index 0000000..4444444
--- /dev/null
+++ b/pkg/new.go
@@ -0,0 +1,2 @@
+package pkg
+const Answer = 42
\ No newline at end of file
diff --git a/img.png b/img.png
index 5555555..6666666 100644
Binary files a/img.png and b/img.png differ
diff --git a/src/a.go b/dst/b.go
similarity index 90%
rename from src/a.go
rename to dst/b.go
index 7777777..8888888 100644
--- a/src/a.go
+++ b/dst/b.go
@@ -1 +1 @@
-package a
+package b
diff --git a/with space.txt b/with space.txt
index 9999999..aaaaaaa 100644
--- a/with space.txt
+++ b/with space.txt
@@ -1 +1 @@
-x
+y
`

func TestParse(t *testing.T) {
	files, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	type want struct {
		path       string
		status     Status
		binary     bool
		adds, dels int
	}
	wants := []want{
		{"main.go", Modified, false, 4, 2},
		{"old/removed.txt", Deleted, false, 0, 1},
		{"pkg/new.go", Added, false, 2, 0},
		{"img.png", Modified, true, 0, 0},
		{"dst/b.go", Renamed, false, 1, 1},
		{"with space.txt", Modified, false, 1, 1},
	}
	if len(files) != len(wants) {
		t.Fatalf("got %d files, want %d", len(files), len(wants))
	}
	for i, w := range wants {
		f := files[i]
		if f.Path() != w.path || f.Status != w.status || f.Binary != w.binary || f.Additions != w.adds || f.Deletions != w.dels {
			t.Errorf("file %d = {%q %s binary=%v +%d -%d}, want %+v", i, f.Path(), f.Status, f.Binary, f.Additions, f.Deletions, w)
		}
	}
	if files[4].OldPath != "src/a.go" {
		t.Errorf("rename old path = %q", files[4].OldPath)
	}
}

func TestCommentable(t *testing.T) {
	files, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	main := files[0]
	// New-side lines: 1-5 in the first hunk, 12-13 in the second.
	for _, line := range []int{1, 2, 3, 4, 5, 12, 13} {
		if !main.Commentable(line) {
			t.Errorf("line %d should be commentable", line)
		}
	}
	for _, line := range []int{0, -1, 6, 11, 14} {
		if main.Commentable(line) {
			t.Errorf("line %d should not be commentable", line)
		}
	}
	if files[1].Commentable(1) {
		t.Error("a deleted file has no new-side lines")
	}
}

func TestAnnotated(t *testing.T) {
	files, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	got := files[0].Annotated()
	for _, want := range []string{
		"    1   package main\n",
		"      - func main() {}\n",
		"    3 + import \"fmt\"\n",
		"   13 + \tb := 3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotated diff lacks %q:\n%s", want, got)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse("not a diff\n"); err == nil {
		t.Error("Parse accepted content without a file header")
	}
	if _, err := Parse("diff --git a/x b/x\n@@ nonsense @@\n"); err == nil {
		t.Error("Parse accepted a malformed hunk header")
	}
}
