package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/charts/chartstest"
	"github.com/mycroft/agent-nexus/internal/llm"
)

func chartFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	srv, repoURL := chartstest.NewRepo(t, "demo", map[string]map[string]string{
		"1.0.0": {"Chart.yaml": "name: demo\n", "values.yaml": "image: demo:1.0\n"},
		"1.1.0": {"Chart.yaml": "name: demo\n", "values.yaml": "image: demo:1.1\nresources: {}\n"},
	})
	f := newFixtureWith(t,
		map[string]string{"versions.yaml": "demo/demo: 1.0.0\n", "repos.go": `const demoRepo = "` + repoURL + `/"` + "\n"},
		map[string]string{"versions.yaml": "demo/demo: 1.1.0\n", "new.go": `const other = "https://charts.example.org/stable"` + "\n"})
	fetcher, err := charts.New(t.TempDir(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.reviewer.Charts = fetcher
	return f, repoURL
}

func TestChartPolicy(t *testing.T) {
	f, repoURL := chartFixture(t)
	ctx := context.Background()
	pr, _ := f.forge.GetPR(ctx, f.req.repo(), 4)
	ws, _, err := f.reviewer.prepare(ctx, f.forge, f.req.repo(), pr, f.remote.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	for url, want := range map[string]string{
		repoURL:                               "",
		repoURL + "/":                         "",
		"https://charts.example.org/stable":   "does not appear in the repository's base branch", // only at the head
		"https://evil.example/" + repoURL[8:]: "does not appear",
		"http://" + strings.TrimPrefix(repoURL, "https://"): "want https:// or oci://",
	} {
		err := ws.chartPolicy(ctx, url)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("chartPolicy(%q) = %v, want %q", url, err, want)
		}
	}
	ws.fetchHosts = []string{"Charts.Example.org"}
	if err := ws.chartPolicy(ctx, "https://charts.example.org/stable"); err != nil {
		t.Errorf("an allowed host: %v", err)
	}
}

func TestDraftReadsCharts(t *testing.T) {
	f, repoURL := chartFixture(t)
	args, _ := json.Marshal(map[string]string{"repo_url": repoURL, "chart": "demo", "from_version": "1.0.0", "to_version": "1.1.0"})
	final, _ := json.Marshal(Output{Summary: "Bumps demo to 1.1.0.", Verdict: "approve", VerdictReason: "Defaults only gain resources."})
	f.llm.replies = []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "1", Type: "function", Function: llm.FunctionCall{Name: "chart_diff", Arguments: string(args)}}}},
		{Role: "assistant", Content: "The bump only adds resources."},
		{Role: "assistant", Content: string(final)},
	}
	if _, _, err := f.reviewer.Draft(context.Background(), f.req, 1, nil); err != nil {
		t.Fatal(err)
	}
	first := f.llm.requests[0]
	if !strings.Contains(first.Messages[0].Content, "chart_files, chart_read and chart_diff read published Helm chart versions") {
		t.Error("the system prompt does not mention the chart tools")
	}
	var names []string
	for _, tool := range first.Tools {
		names = append(names, tool.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "chart_files,chart_read,chart_diff") {
		t.Errorf("tools = %v", names)
	}
	result := f.llm.requests[1].Messages[len(f.llm.requests[1].Messages)-1]
	if result.Role != "tool" || !strings.Contains(result.Content, "-image: demo:1.0\n+image: demo:1.1\n+resources: {}") {
		t.Errorf("chart_diff result = %+v", result)
	}
}

func TestToolsWithoutCharts(t *testing.T) {
	f := newFixture(t)
	f.script(sampleOutput)
	if _, _, err := f.reviewer.Draft(context.Background(), f.req, 1, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.llm.requests[0].Messages[0].Content, "read published Helm chart versions") || len(f.llm.requests[0].Tools) != 5 {
		t.Error("chart tools offered without a fetcher")
	}
}
