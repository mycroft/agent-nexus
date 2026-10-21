// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/models"
)

// Config holds all environment-derived runtime settings.
type Config struct {
	// ListenAddr serves the public API (events, jobs, healthz).
	ListenAddr string
	// MetricsAddr serves /metrics. It is kept off the public listener so the
	// Ingress never exposes it.
	MetricsAddr string
	// UIAddr serves the jobs UI, which has no authentication of its own:
	// expose it only behind an authenticating proxy.
	UIAddr string
	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string

	// DBPath is the SQLite database file. ":memory:" is accepted for tests.
	DBPath string
	// RepoCacheDir holds one bare git repository per reviewed repo.
	RepoCacheDir string
	// ChartCacheDir caches the Helm chart packages and indexes the agent
	// reads.
	ChartCacheDir string
	// FetchAllowedHosts are hosts the agent may read charts from even
	// when the reviewed repository does not reference them.
	FetchAllowedHosts []string
	// SkillsDir, if set, replaces the embedded skills (for iterating on a
	// skill without rebuilding).
	SkillsDir string
	// MCPConfig is the path of the MCP servers' YAML configuration, parsed
	// by mcp.LoadConfig. Empty configures none.
	MCPConfig string

	// APIKeys is the raw "name=key,name=key" list accepted by the event API.
	APIKeys string
	// AllowedOwners restricts which repository owners may be reviewed
	// (case-insensitive). Empty allows none.
	AllowedOwners []string
	// EventsPerMinute is the sustained per-API-key event rate.
	EventsPerMinute int
	// ReviewModes is the raw "pattern=mode,..." central review mode
	// configuration, parsed by review.ParseModeRules.
	ReviewModes string

	// ModelsConfig is the path of the models' YAML configuration, parsed
	// by models.LoadConfig. Empty configures one model from the LLM_*
	// variables.
	ModelsConfig string
	// Models are the LLMs jobs run on, from ModelsConfig or LLM_*.
	Models []models.Model

	// AgentMaxTurns bounds the tool-calling loop of one job.
	AgentMaxTurns int
	// JobTimeout bounds one job end to end.
	JobTimeout time.Duration
	// TranscriptRetention is how long per-step agent transcripts are kept.
	TranscriptRetention time.Duration

	GiteaURL   string
	GiteaUser  string
	GiteaToken string

	GitHubAppID         int64
	GitHubAppPrivateKey string
	GitHubAPIURL        string
	GitHubServerURL     string

	// ZulipURL, ZulipBotEmail and ZulipBotAPIKey let agent-nexus answer
	// questions asked on Zulip, as the outgoing webhook bot whose requests
	// carry ZulipWebhookToken.
	ZulipURL          string
	ZulipBotEmail     string
	ZulipBotAPIKey    string
	ZulipWebhookToken string
}

// Load reads configuration from the environment and applies defaults.
func Load() (Config, error) {
	c := Config{
		ListenAddr:          getEnv("LISTEN_ADDR", ":8080"),
		MetricsAddr:         getEnv("METRICS_ADDR", ":9090"),
		UIAddr:              getEnv("UI_ADDR", ":8081"),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
		DBPath:              getEnv("DB_PATH", "agent-nexus.db"),
		RepoCacheDir:        getEnv("REPO_CACHE_DIR", "repos"),
		ChartCacheDir:       getEnv("CHART_CACHE_DIR", "charts"),
		FetchAllowedHosts:   splitList(os.Getenv("FETCH_ALLOWED_HOSTS")),
		SkillsDir:           os.Getenv("SKILLS_DIR"),
		MCPConfig:           os.Getenv("MCP_CONFIG"),
		APIKeys:             os.Getenv("API_KEYS"),
		AllowedOwners:       splitList(os.Getenv("ALLOWED_OWNERS")),
		ReviewModes:         os.Getenv("REVIEW_MODES"),
		ModelsConfig:        os.Getenv("MODELS_CONFIG"),
		GiteaURL:            strings.TrimRight(os.Getenv("GITEA_URL"), "/"),
		GiteaUser:           os.Getenv("GITEA_USER"),
		GiteaToken:          os.Getenv("GITEA_TOKEN"),
		GitHubAppPrivateKey: os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAPIURL:        strings.TrimRight(getEnv("GITHUB_API_URL", "https://api.github.com"), "/"),
		GitHubServerURL:     strings.TrimRight(getEnv("GITHUB_SERVER_URL", "https://github.com"), "/"),
		ZulipURL:            strings.TrimRight(os.Getenv("ZULIP_URL"), "/"),
		ZulipBotEmail:       os.Getenv("ZULIP_BOT_EMAIL"),
		ZulipBotAPIKey:      os.Getenv("ZULIP_BOT_API_KEY"),
		ZulipWebhookToken:   os.Getenv("ZULIP_WEBHOOK_TOKEN"),
	}

	var err error
	if c.EventsPerMinute, err = getEnvPositiveInt("EVENTS_PER_MINUTE", 30); err != nil {
		return Config{}, err
	}
	if c.AgentMaxTurns, err = getEnvPositiveInt("AGENT_MAX_TURNS", 30); err != nil {
		return Config{}, err
	}
	if c.JobTimeout, err = getEnvDuration("JOB_TIMEOUT", 30*time.Minute); err != nil {
		return Config{}, err
	}
	if c.TranscriptRetention, err = getEnvDuration("TRANSCRIPT_RETENTION", 30*24*time.Hour); err != nil {
		return Config{}, err
	}
	if v := os.Getenv("GITHUB_APP_ID"); v != "" {
		if c.GitHubAppID, err = strconv.ParseInt(v, 10, 64); err != nil {
			return Config{}, fmt.Errorf("parse GITHUB_APP_ID=%q: %w", v, err)
		}
	}

	if c.Models, err = loadModels(c.ModelsConfig); err != nil {
		return Config{}, err
	}
	if err := requireAllOrNone("Gitea", map[string]string{
		"GITEA_URL":   c.GiteaURL,
		"GITEA_USER":  c.GiteaUser,
		"GITEA_TOKEN": c.GiteaToken,
	}); err != nil {
		return Config{}, err
	}
	githubID := ""
	if c.GitHubAppID != 0 {
		githubID = "set"
	}
	if err := requireAllOrNone("the GitHub App", map[string]string{
		"GITHUB_APP_ID":          githubID,
		"GITHUB_APP_PRIVATE_KEY": c.GitHubAppPrivateKey,
	}); err != nil {
		return Config{}, err
	}
	if !c.GiteaEnabled() && !c.GitHubEnabled() {
		return Config{}, errors.New("configure at least one forge (GITEA_* or GITHUB_APP_*)")
	}
	if err := requireAllOrNone("Zulip", map[string]string{
		"ZULIP_URL":           c.ZulipURL,
		"ZULIP_BOT_EMAIL":     c.ZulipBotEmail,
		"ZULIP_BOT_API_KEY":   c.ZulipBotAPIKey,
		"ZULIP_WEBHOOK_TOKEN": c.ZulipWebhookToken,
	}); err != nil {
		return Config{}, err
	}
	return c, nil
}

// llmVariables configure the one model of a deployment without a models
// file.
var llmVariables = []string{
	"LLM_BASE_URL", "LLM_API_KEY", "LLM_MODEL", "LLM_CONTEXT_TOKENS",
	"LLM_THINKING_BUDGET", "LLM_CONCURRENCY", "LLM_REQUEST_TIMEOUT",
}

// loadModels reads the models file at path, or without one, the model
// the LLM_* variables configure. Both at once would leave one ignored.
func loadModels(path string) ([]models.Model, error) {
	if path != "" {
		for _, key := range llmVariables {
			if os.Getenv(key) != "" {
				return nil, fmt.Errorf("%s and MODELS_CONFIG are exclusive: configure the models in MODELS_CONFIG", key)
			}
		}
		return models.LoadConfig(path)
	}
	m := models.Model{BaseURL: os.Getenv("LLM_BASE_URL"), Model: os.Getenv("LLM_MODEL")}
	if m.BaseURL == "" {
		return nil, errors.New("LLM_BASE_URL is required (or MODELS_CONFIG)")
	}
	if m.Model == "" {
		return nil, errors.New("LLM_MODEL is required")
	}
	var err error
	if m.ContextTokens, err = getEnvInt("LLM_CONTEXT_TOKENS", 0); err != nil {
		return nil, err
	}
	if m.ThinkingBudget, err = getEnvInt("LLM_THINKING_BUDGET", 0); err != nil {
		return nil, err
	}
	if m.Concurrency, err = getEnvPositiveInt("LLM_CONCURRENCY", 1); err != nil {
		return nil, err
	}
	if m.RequestTimeout, err = getEnvDuration("LLM_REQUEST_TIMEOUT", models.DefaultRequestTimeout); err != nil {
		return nil, err
	}
	return models.Default(m, os.Getenv("LLM_API_KEY"))
}

// GiteaEnabled reports whether the Gitea forge is configured.
func (c Config) GiteaEnabled() bool { return c.GiteaURL != "" }

// GitHubEnabled reports whether the GitHub App forge is configured.
func (c Config) GitHubEnabled() bool { return c.GitHubAppID != 0 }

// ZulipEnabled reports whether the Zulip bot is configured.
func (c Config) ZulipEnabled() bool { return c.ZulipURL != "" }

// requireAllOrNone checks that the settings of what are all set, or none.
func requireAllOrNone(what string, values map[string]string) error {
	set := 0
	for _, v := range values {
		if v != "" {
			set++
		}
	}
	if set == 0 || set == len(values) {
		return nil
	}
	for key, v := range values {
		if v == "" {
			return fmt.Errorf("%s is required when %s is configured", key, what)
		}
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as integer: %w", key, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %d", key, n)
	}
	return n, nil
}

func getEnvPositiveInt(key string, fallback int) (int, error) {
	n, err := getEnvInt(key, fallback)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return n, nil
}

func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as duration: %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}
