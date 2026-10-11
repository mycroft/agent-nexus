package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/charts/chartstest"
	"github.com/mycroft/agent-nexus/internal/remote"
)

func chartTools(t *testing.T, allow ChartPolicy) (map[string]Tool, string) {
	t.Helper()
	srv, repo := chartstest.NewRepo(t, "demo", map[string]map[string]string{
		"1.0.0": {"Chart.yaml": "name: demo\nversion: 1.0.0\n", "values.yaml": "replicas: 1\nimage: demo:1.0\n", "templates/svc.yaml": "kind: Service\n", "README.md": "old\n"},
		"1.1.0": {"Chart.yaml": "name: demo\nversion: 1.1.0\n", "values.yaml": "replicas: 1\nimage: demo:1.1\n", "templates/svc.yaml": "kind: Service\n", "templates/hpa.yaml": "kind: HorizontalPodAutoscaler\n"},
	})
	f, err := charts.New(t.TempDir(), remote.New(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Tool{}
	for _, tool := range Charts(f, allow) {
		byName[tool.Name] = tool
	}
	return byName, repo
}

func call(t *testing.T, tool Tool, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tool.Run(context.Background(), raw)
}

func TestChartTools(t *testing.T) {
	allowAll := func(context.Context, string) error { return nil }
	tools, repo := chartTools(t, allowAll)

	out, err := call(t, tools["chart_files"], map[string]any{"repo_url": repo, "chart": "demo", "version": "1.1.0"})
	if err != nil || out != "demo 1.1.0: 4 files\nChart.yaml (26 bytes)\ntemplates/hpa.yaml (30 bytes)\ntemplates/svc.yaml (14 bytes)\nvalues.yaml (28 bytes)\n" {
		t.Errorf("chart_files = %q, %v", out, err)
	}
	out, err = call(t, tools["chart_read"], map[string]any{"repo_url": repo, "chart": "demo", "version": "1.0.0", "path": "./values.yaml", "start_line": 2})
	if err != nil || out != "    2  image: demo:1.0\n(showing lines 2-2 of 2)\n" {
		t.Errorf("chart_read = %q, %v", out, err)
	}
	if _, err := call(t, tools["chart_read"], map[string]any{"repo_url": repo, "chart": "demo", "version": "1.0.0", "path": "../../etc/passwd"}); err == nil || !strings.Contains(err.Error(), `no file "etc/passwd"`) {
		t.Errorf("chart_read outside the chart: %v", err)
	}

	out, err = call(t, tools["chart_diff"], map[string]any{"repo_url": repo, "chart": "demo", "from_version": "1.0.0", "to_version": "1.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	// Metadata and defaults come first, then templates, then the rest.
	wantList := "demo 1.0.0 → 1.1.0: 4 files differ\n  changed Chart.yaml\n  changed values.yaml\n  removed README.md\n  added templates/hpa.yaml\n"
	if !strings.HasPrefix(out, wantList) {
		t.Errorf("chart_diff list:\n%s", out)
	}
	for _, want := range []string{"--- a/values.yaml\n+++ b/values.yaml\n", "-image: demo:1.0\n+image: demo:1.1\n", "+++ b/templates/hpa.yaml\n", "-old\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("chart_diff lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "svc.yaml") {
		t.Errorf("an unchanged file is listed:\n%s", out)
	}
	out, err = call(t, tools["chart_diff"], map[string]any{"repo_url": repo, "chart": "demo", "from_version": "1.0.0", "to_version": "1.1.0", "path": "templates/svc.yaml"})
	if err != nil || out != "templates/svc.yaml is the same in 1.0.0 and 1.1.0" {
		t.Errorf("chart_diff of an unchanged file = %q, %v", out, err)
	}
}

func TestChartToolsAskThePolicy(t *testing.T) {
	var asked []string
	deny := func(_ context.Context, repoURL string) error {
		asked = append(asked, repoURL)
		return errors.New("not used by this repository")
	}
	tools, repo := chartTools(t, deny)
	for name, args := range map[string]map[string]any{
		"chart_files": {"repo_url": repo, "chart": "demo", "version": "1.0.0"},
		"chart_read":  {"repo_url": repo, "chart": "demo", "version": "1.0.0", "path": "values.yaml"},
		"chart_diff":  {"repo_url": repo, "chart": "demo", "from_version": "1.0.0", "to_version": "1.1.0"},
	} {
		if _, err := call(t, tools[name], args); err == nil || !strings.Contains(err.Error(), "not used by this repository") {
			t.Errorf("%s: err = %v, want the policy's", name, err)
		}
	}
	if len(asked) != 3 {
		t.Errorf("policy asked %d times, want once per call", len(asked))
	}
}
