package models

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseConfig(t *testing.T) {
	t.Setenv("MODELS_TEST_KEY", "s3cret")
	models, err := ParseConfig([]byte(`
models:
  - name: qwen
    baseURL: https://llm.example.com/v1
    model: Qwen 3.8
    apiKeyEnv: MODELS_TEST_KEY
    concurrency: 2
    contextTokens: 196608
    thinkingBudget: 8192
    requestTimeout: 15m
  - name: gpu
    jobs: [chat]
    local: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	qwen, gpu := models[0], models[1]
	if qwen.Name != "qwen" || qwen.Model != "Qwen 3.8" || qwen.Concurrency != 2 || !qwen.Local || qwen.APIKey() != "s3cret" ||
		qwen.ContextTokens != 196608 || qwen.ThinkingBudget != 8192 || qwen.RequestTimeout != 15*time.Minute || !qwen.Runnable() {
		t.Errorf("qwen = %+v", qwen)
	}
	if !slices.Equal(qwen.Jobs, Jobs) {
		t.Errorf("qwen jobs = %v, want every job", qwen.Jobs)
	}
	// Without a baseURL, the model is scheduled here but run elsewhere.
	if gpu.Runnable() || gpu.Local || gpu.Concurrency != 1 || !gpu.Serves(JobChat) || gpu.Serves(JobReview) ||
		gpu.RequestTimeout != DefaultRequestTimeout || gpu.APIKey() != "" {
		t.Errorf("gpu = %+v", gpu)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":             ``,
		"no models":         `models: []`,
		"unknown key":       "models:\n  - name: a\n    concurency: 2",
		"bad name":          "models:\n  - name: Qwen",
		"duplicate":         "models:\n  - name: a\n  - name: a",
		"relative url":      "models:\n  - name: a\n    baseURL: llm/v1\n    model: m",
		"url without model": "models:\n  - name: a\n    baseURL: https://llm/v1",
		"zero concurrency":  "models:\n  - name: a\n    concurrency: 0",
		"unknown job":       "models:\n  - name: a\n    jobs: [triage]",
		"negative tokens":   "models:\n  - name: a\n    contextTokens: -1",
		"bad timeout":       "models:\n  - name: a\n    requestTimeout: soon",
		"unset key":         "models:\n  - name: a\n    apiKeyEnv: MODELS_TEST_UNSET",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(raw)); err == nil {
				t.Error("ParseConfig succeeded")
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte("models:\n  - name: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if models, err := LoadConfig(p); err != nil || len(models) != 1 || models[0].Name != "a" {
		t.Errorf("LoadConfig = %+v, %v", models, err)
	}
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("LoadConfig read a missing file")
	}
	if err := os.WriteFile(p, []byte("models:\n  - name: A\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("LoadConfig error = %v, want it to name the file", err)
	}
}

func TestDefault(t *testing.T) {
	models, err := Default(Model{BaseURL: "http://llm/v1", Model: "m", Concurrency: 3}, "key")
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if len(models) != 1 || m.Name != DefaultName || !m.Local || m.APIKey() != "key" || m.Concurrency != 3 ||
		m.RequestTimeout != DefaultRequestTimeout || !slices.Equal(m.Jobs, Jobs) {
		t.Errorf("Default = %+v", models)
	}
	if _, err := Default(Model{BaseURL: "http://llm/v1", Model: "m"}, ""); err == nil {
		t.Error("Default accepted a zero concurrency")
	}
}
