package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/models"
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
	if c.AgentMaxTurns != 30 || c.JobTimeout != 30*time.Minute {
		t.Errorf("defaults: %+v", c)
	}
	// Without a models file, LLM_* configure the one model.
	if len(c.Models) != 1 {
		t.Fatalf("Models = %+v, want the default one", c.Models)
	}
	if m := c.Models[0]; m.Name != models.DefaultName || m.BaseURL != "http://llm/v1" || m.Model != "m" || m.Concurrency != 1 ||
		m.RequestTimeout != 10*time.Minute || m.ThinkingBudget != 0 || m.ContextTokens != 0 || !m.Local {
		t.Errorf("default model: %+v", m)
	}
	t.Setenv("LLM_THINKING_BUDGET", "6144")
	t.Setenv("LLM_CONCURRENCY", "2")
	t.Setenv("LLM_API_KEY", "key")
	if c, err := Load(); err != nil || c.Models[0].ThinkingBudget != 6144 || c.Models[0].Concurrency != 2 || c.Models[0].APIKey() != "key" {
		t.Errorf("LLM_* = %+v, %v", c.Models, err)
	}
	if c.ZulipEnabled() {
		t.Error("Zulip enabled without its settings")
	}
	if c.MCPConfig != "" {
		t.Errorf("MCPConfig = %q, want none", c.MCPConfig)
	}
	t.Setenv("MCP_CONFIG", "/etc/agent-nexus/mcp/config.yaml")
	if c, err := Load(); err != nil || c.MCPConfig != "/etc/agent-nexus/mcp/config.yaml" {
		t.Errorf("MCP_CONFIG = %q, %v", c.MCPConfig, err)
	}
	t.Setenv("ZULIP_URL", "https://zulip.example.com/")
	t.Setenv("ZULIP_BOT_EMAIL", "agent-nexus-bot@zulip.example.com")
	t.Setenv("ZULIP_BOT_API_KEY", "key")
	t.Setenv("ZULIP_WEBHOOK_TOKEN", "token")
	if c, err := Load(); err != nil || !c.ZulipEnabled() || c.ZulipURL != "https://zulip.example.com" {
		t.Errorf("Zulip: %+v, %v", c, err)
	}
}

func TestLoadRunner(t *testing.T) {
	setBase(t)
	c, err := Load()
	if host, _ := os.Hostname(); err != nil || c.RunnerName != host || c.ControllerURL != "" {
		t.Errorf("runner: %q, %q, %v; want the host name by default", c.RunnerName, c.ControllerURL, err)
	}
	t.Setenv("CONTROLLER_URL", "https://agent-nexus.example.com")
	t.Setenv("RUNNER_TOKEN", "token")
	t.Setenv("RUNNER_NAME", "box-1")
	// A runner needs no webhook token: only the controller receives the
	// webhook.
	t.Setenv("ZULIP_URL", "https://zulip.example.com")
	t.Setenv("ZULIP_BOT_EMAIL", "bot@zulip.example.com")
	t.Setenv("ZULIP_BOT_API_KEY", "key")
	c, err = Load()
	if err != nil || c.ControllerURL != "https://agent-nexus.example.com" || c.RunnerToken != "token" || c.RunnerName != "box-1" || !c.ZulipEnabled() {
		t.Errorf("runner: %+v, %v", c, err)
	}
}

func TestLoadModelsConfig(t *testing.T) {
	setBase(t)
	p := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(p, []byte("models:\n  - name: qwen\n    baseURL: http://llm/v1\n    model: q\n  - name: gpu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MODELS_CONFIG", p)
	// MODELS_CONFIG replaces LLM_*: setting both is an error.
	if _, err := Load(); err == nil {
		t.Error("Load accepted MODELS_CONFIG with LLM_BASE_URL")
	}
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 2 || c.Models[0].Name != "qwen" || c.Models[1].Runnable() {
		t.Errorf("Models = %+v", c.Models)
	}
	t.Setenv("MODELS_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
	if _, err := Load(); err == nil {
		t.Error("Load accepted a missing models file")
	}
}

func TestLoadErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"partial gitea":   {"GITEA_TOKEN": ""},
		"partial github":  {"GITHUB_APP_ID": "12"},
		"partial zulip":   {"ZULIP_URL": "https://zulip.example.com", "ZULIP_BOT_EMAIL": "bot@example.com"},
		"no forge":        {"GITEA_URL": "", "GITEA_USER": "", "GITEA_TOKEN": ""},
		"bad duration":    {"JOB_TIMEOUT": "soon"},
		"zero workers":    {"LLM_CONCURRENCY": "0"},
		"negative tokens": {"LLM_CONTEXT_TOKENS": "-1"},
		"negative budget": {"LLM_THINKING_BUDGET": "-1"},
		"invalid budget":  {"LLM_THINKING_BUDGET": "lots"},
		"no model":        {"LLM_MODEL": ""},
		"no llm":          {"LLM_BASE_URL": ""},
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
