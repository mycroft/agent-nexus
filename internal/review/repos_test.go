package review

import (
	"context"
	"strings"
	"testing"
)

func TestOpenRepoPolicy(t *testing.T) {
	f := newFixtureWith(t,
		map[string]string{"charts.go": "// Source: https://git.example.com/o/lib (private)\n// Clone: https://git.example.com/o/tool.git\n"},
		map[string]string{"new.go": "// https://git.example.com/o/added\n"})
	ctx := context.Background()
	pr, _ := f.forge.GetPR(ctx, f.req.repo(), 4)
	ws, _, err := f.reviewer.prepare(ctx, f.forge, f.req.repo(), pr, f.remote.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	for repo, want := range map[string]string{
		"o/calc":     "", // the repository under review
		"o/lib":      "",
		"o/tool.git": "",                // referenced by its clone URL
		"o/li":       "does not appear", // a prefix of a referenced name
		"x/lib":      "does not appear",
		"o/added":    "does not appear", // only at the head
		"../etc":     "invalid repository",
		"o/lib/x":    "invalid repository",
	} {
		// The fake forge serves the fixture's remote for any repository.
		_, commits, _, err := ws.openRepo(ctx, repo, []string{"main"})
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("openRepo(%q) = %v, want %q", repo, err, want)
		}
		if want == "" && commits["main"] != f.remote.BaseSHA {
			t.Errorf("openRepo(%q) commits = %v", repo, commits)
		}
	}
	if !strings.Contains(ws.toolsSentence(), "repo_log and repo_read read other repositories of this forge") {
		t.Error("the tools sentence does not mention the repo tools")
	}
}
