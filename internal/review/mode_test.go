package review

import (
	"testing"

	"github.com/mycroft/agent-nexus/internal/forge"
)

func TestParseModeRules(t *testing.T) {
	rules, err := ParseModeRules(" Mycroft/K8s-Home=verdict, mycroft/*=comment ,")
	if err != nil {
		t.Fatal(err)
	}
	want := []ModeRule{{"mycroft/k8s-home", ModeVerdict}, {"mycroft/*", ModeComment}}
	if len(rules) != 2 || rules[0] != want[0] || rules[1] != want[1] {
		t.Errorf("rules = %+v, want %+v", rules, want)
	}
	if rules, err := ParseModeRules(""); err != nil || rules != nil {
		t.Errorf("empty = %v, %v", rules, err)
	}
	for _, bad := range []string{"mycroft/x", "noslash=verdict", "mycroft/x=approve", "mycroft/[=comment"} {
		if _, err := ParseModeRules(bad); err == nil {
			t.Errorf("ParseModeRules(%q) accepted", bad)
		}
	}
}

func TestParseRepoConfig(t *testing.T) {
	for content, want := range map[string]Mode{
		"":                             "",
		"# nothing yet\n":              "",
		"review:\n  mode: verdict\n":   ModeVerdict,
		"review:\n  mode: comment\n":   ModeComment,
		"review: {}\n":                 "",
		"review:\n  mode: \"verdict\"": ModeVerdict,
	} {
		got, err := parseRepoConfig([]byte(content))
		if err != nil || got != want {
			t.Errorf("parseRepoConfig(%q) = %q, %v; want %q", content, got, err, want)
		}
	}
	for _, bad := range []string{"review:\n  mode: approve\n", "review:\n  moed: verdict\n", "reviews: {}\n", "review: [\n"} {
		if _, err := parseRepoConfig([]byte(bad)); err == nil {
			t.Errorf("parseRepoConfig(%q) accepted", bad)
		}
	}
}

func TestResolveMode(t *testing.T) {
	rules := []ModeRule{
		{"mycroft/*", ModeVerdict},
		{"mycroft/pinned", ModeComment},
		{"*/*", ModeComment},
	}
	for _, tc := range []struct {
		repo   string
		file   Mode
		want   Mode
		source string
	}{
		// An exact central rule wins over the repository file, even
		// when listed after a matching pattern.
		{"mycroft/pinned", ModeVerdict, ModeComment, "central rule mycroft/pinned"},
		// The repository file wins over central patterns.
		{"mycroft/other", ModeComment, ModeComment, RepoConfigFile},
		{"someone/repo", ModeVerdict, ModeVerdict, RepoConfigFile},
		// Without a file, the first matching pattern applies.
		{"Mycroft/Other", "", ModeVerdict, "central rule mycroft/*"},
		{"someone/repo", "", ModeComment, "central rule */*"},
	} {
		repo, _ := forge.ParseRepo(tc.repo)
		got, source := resolveMode(rules, repo, tc.file)
		if got != tc.want || source != tc.source {
			t.Errorf("resolveMode(%s, file=%q) = %s (%s), want %s (%s)", tc.repo, tc.file, got, source, tc.want, tc.source)
		}
	}
	repo, _ := forge.ParseRepo("a/b")
	if got, source := resolveMode(nil, repo, ""); got != ModeComment || source != "default" {
		t.Errorf("no rules = %s (%s)", got, source)
	}
}

func TestVerdictEvent(t *testing.T) {
	finding := func(sev string) Finding { return Finding{Severity: sev} }
	for _, tc := range []struct {
		verdict  string
		findings []Finding
		want     forge.Event
		note     bool
	}{
		{"approve", nil, forge.EventApprove, false},
		{"approve", []Finding{finding("nit"), finding("question")}, forge.EventApprove, false},
		{"approve", []Finding{finding("important")}, forge.EventComment, true},
		{"request_changes", []Finding{finding("blocking")}, forge.EventRequestChanges, false},
		{"request_changes", []Finding{finding("important")}, forge.EventComment, true},
		{"comment", []Finding{finding("blocking")}, forge.EventComment, false},
	} {
		got, note := verdictEvent(Output{Verdict: tc.verdict, Findings: tc.findings})
		if got != tc.want || (note != "") != tc.note {
			t.Errorf("verdictEvent(%s, %v) = %s, %q", tc.verdict, tc.findings, got, note)
		}
	}
}

func TestPostAttempts(t *testing.T) {
	sha := "abc"
	inline := []forge.Comment{{Path: "a", Line: 1, Body: "c"}}
	for _, tc := range []struct {
		review forge.NewReview
		want   []string
	}{
		{forge.NewReview{Event: forge.EventComment}, []string{"comment/0"}},
		{forge.NewReview{Event: forge.EventComment, Comments: inline}, []string{"comment/1", "comment/0"}},
		{forge.NewReview{Event: forge.EventApprove}, []string{"approve/0", "comment/0"}},
		{forge.NewReview{Event: forge.EventRequestChanges, Comments: inline}, []string{"request_changes/1", "request_changes/0", "comment/0"}},
	} {
		var got []string
		for _, a := range postAttempts(&Draft{HeadSHA: sha, Review: tc.review, FallbackBody: "fb"}) {
			got = append(got, string(a.Event)+"/"+itoa(len(a.Comments)))
		}
		if len(got) != len(tc.want) {
			t.Errorf("attempts for %+v = %v, want %v", tc.review, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("attempts for %+v = %v, want %v", tc.review, got, tc.want)
				break
			}
		}
	}
}
