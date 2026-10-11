package config

import (
	"testing"
	"time"
)

func setBase(t *testing.T) {
	t.Helper()
	t.Setenv("LLM_BASE_URL", "http://llm/v1")
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("GITEA_URL", "https://git.example.com/")
	t.Setenv("GITEA_USER", "bot")
	t.Setenv("GITEA_TOKEN", "tok")
}

func TestLoadDefaults(t *testing.T) {
	setBase(t)
	t.Setenv("ALLOWED_OWNERS", " mycroft, other ,")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GiteaURL != "https://git.example.com" || !c.GiteaEnabled() || c.GitHubEnabled() {
		t.Errorf("forges: %+v", c)
	}
	if len(c.AllowedOwners) != 2 || c.AllowedOwners[1] != "other" {
		t.Errorf("AllowedOwners = %q", c.AllowedOwners)
	}
	if c.LLMConcurrency != 1 || c.AgentMaxTurns != 30 || c.LLMRequestTimeout != 10*time.Minute || c.JobTimeout != 30*time.Minute || c.LLMThinkingBudget != 0 {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("LLM_THINKING_BUDGET", "6144")
	if c, err := Load(); err != nil || c.LLMThinkingBudget != 6144 {
		t.Errorf("LLM_THINKING_BUDGET = %d, %v", c.LLMThinkingBudget, err)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"partial gitea":   {"GITEA_TOKEN": ""},
		"partial github":  {"GITHUB_APP_ID": "12"},
		"no forge":        {"GITEA_URL": "", "GITEA_USER": "", "GITEA_TOKEN": ""},
		"bad duration":    {"JOB_TIMEOUT": "soon"},
		"zero workers":    {"LLM_CONCURRENCY": "0"},
		"negative tokens": {"LLM_CONTEXT_TOKENS": "-1"},
		"negative budget": {"LLM_THINKING_BUDGET": "-1"},
		"invalid budget":  {"LLM_THINKING_BUDGET": "lots"},
		"no model":        {"LLM_MODEL": ""},
	} {
		t.Run(name, func(t *testing.T) {
			setBase(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Error("Load succeeded")
			}
		})
	}
}
