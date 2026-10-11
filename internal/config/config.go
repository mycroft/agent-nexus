// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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

	LLMBaseURL string
	LLMAPIKey  string
	LLMModel   string
	// LLMContextTokens overrides the context size discovered from
	// llama-server's /props endpoint. Zero means discover.
	LLMContextTokens int
	// LLMConcurrency is the number of jobs run in parallel; match
	// llama-server's --parallel.
	LLMConcurrency int
	// LLMRequestTimeout bounds one chat completion, including the time
	// llama-server keeps it queued while all slots are busy.
	LLMRequestTimeout time.Duration

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
		APIKeys:             os.Getenv("API_KEYS"),
		AllowedOwners:       splitList(os.Getenv("ALLOWED_OWNERS")),
		ReviewModes:         os.Getenv("REVIEW_MODES"),
		LLMBaseURL:          os.Getenv("LLM_BASE_URL"),
		LLMAPIKey:           os.Getenv("LLM_API_KEY"),
		LLMModel:            os.Getenv("LLM_MODEL"),
		GiteaURL:            strings.TrimRight(os.Getenv("GITEA_URL"), "/"),
		GiteaUser:           os.Getenv("GITEA_USER"),
		GiteaToken:          os.Getenv("GITEA_TOKEN"),
		GitHubAppPrivateKey: os.Getenv("GITHUB_APP_PRIVATE_KEY"),
		GitHubAPIURL:        strings.TrimRight(getEnv("GITHUB_API_URL", "https://api.github.com"), "/"),
		GitHubServerURL:     strings.TrimRight(getEnv("GITHUB_SERVER_URL", "https://github.com"), "/"),
	}

	var err error
	if c.EventsPerMinute, err = getEnvPositiveInt("EVENTS_PER_MINUTE", 30); err != nil {
		return Config{}, err
	}
	if c.LLMContextTokens, err = getEnvInt("LLM_CONTEXT_TOKENS", 0); err != nil {
		return Config{}, err
	}
	if c.LLMConcurrency, err = getEnvPositiveInt("LLM_CONCURRENCY", 1); err != nil {
		return Config{}, err
	}
	if c.AgentMaxTurns, err = getEnvPositiveInt("AGENT_MAX_TURNS", 30); err != nil {
		return Config{}, err
	}
	if c.LLMRequestTimeout, err = getEnvDuration("LLM_REQUEST_TIMEOUT", 10*time.Minute); err != nil {
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

	if c.LLMBaseURL == "" {
		return Config{}, errors.New("LLM_BASE_URL is required")
	}
	if c.LLMModel == "" {
		return Config{}, errors.New("LLM_MODEL is required")
	}
	if err := requireAllOrNone(map[string]string{
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
	if err := requireAllOrNone(map[string]string{
		"GITHUB_APP_ID":          githubID,
		"GITHUB_APP_PRIVATE_KEY": c.GitHubAppPrivateKey,
	}); err != nil {
		return Config{}, err
	}
	if !c.GiteaEnabled() && !c.GitHubEnabled() {
		return Config{}, errors.New("configure at least one forge (GITEA_* or GITHUB_APP_*)")
	}
	return c, nil
}

// GiteaEnabled reports whether the Gitea forge is configured.
func (c Config) GiteaEnabled() bool { return c.GiteaURL != "" }

// GitHubEnabled reports whether the GitHub App forge is configured.
func (c Config) GitHubEnabled() bool { return c.GitHubAppID != 0 }

func requireAllOrNone(values map[string]string) error {
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
			return fmt.Errorf("%s is required when its forge is configured", key)
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
