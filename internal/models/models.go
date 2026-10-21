// Package models configures the LLMs jobs run on: their endpoints, and
// how many jobs each may run at once across the controller and its
// runners.
package models

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

// The jobs a model may serve. They are the job names of internal/mcp.
const (
	JobChat       = "chat"
	JobReview     = "review"
	JobDiscussion = "discussion"
)

// Jobs lists every job, in the order a model serves them by default.
var Jobs = []string{JobReview, JobDiscussion, JobChat}

// DefaultName names the model the LLM_* variables configure.
const DefaultName = "default"

// DefaultRequestTimeout bounds one completion when the configuration sets
// none.
const DefaultRequestTimeout = 10 * time.Minute

// namePattern is that of a model name, as of an MCP server name.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// Model configures one LLM.
type Model struct {
	// Name names the model in jobs, metrics and the UI.
	Name string
	// BaseURL is the OpenAI-compatible API root. A process runs the
	// model's jobs only if it has one: the controller can leave a model
	// it cannot reach to its runners.
	BaseURL string
	// Model is the upstream model identifier, sent with each request and
	// shown in the review footer.
	Model string
	// Concurrency bounds the model's jobs running at once: on the
	// controller across every runner, on a runner its own.
	Concurrency int
	// Jobs are the jobs the model serves (read by the controller).
	Jobs []string
	// Local tells the controller to run the model's jobs itself, as well
	// as through its runners.
	Local bool
	// ContextTokens overrides the context size discovered from
	// llama-server's /props. Zero means discover.
	ContextTokens int
	// ThinkingBudget caps the model's reasoning per request, in tokens.
	// Zero leaves the server's default.
	ThinkingBudget int
	// RequestTimeout bounds one completion, including the time the server
	// keeps it queued.
	RequestTimeout time.Duration
	// APIKeyEnv names the environment variable holding the API key.
	APIKeyEnv string

	apiKey string
}

// APIKey returns the model's API key, if any.
func (m Model) APIKey() string { return m.apiKey }

// Serves reports whether the model serves job.
func (m Model) Serves(job string) bool { return slices.Contains(m.Jobs, job) }

// Runnable reports whether a process configured with m can run its jobs.
func (m Model) Runnable() bool { return m.BaseURL != "" }

// file is the configuration file's layout.
type file struct {
	Models []struct {
		Name           string   `yaml:"name"`
		BaseURL        string   `yaml:"baseURL"`
		Model          string   `yaml:"model"`
		Concurrency    *int     `yaml:"concurrency"`
		Jobs           []string `yaml:"jobs"`
		Local          *bool    `yaml:"local"`
		ContextTokens  int      `yaml:"contextTokens"`
		ThinkingBudget int      `yaml:"thinkingBudget"`
		RequestTimeout string   `yaml:"requestTimeout"`
		APIKeyEnv      string   `yaml:"apiKeyEnv"`
	} `yaml:"models"`
}

// LoadConfig reads the models configured in the YAML file at p.
func LoadConfig(p string) ([]Model, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("models config: %w", err)
	}
	models, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("models config %s: %w", p, err)
	}
	return models, nil
}

// ParseConfig parses and checks a configuration file. Unknown keys are
// errors, so a typo does not silently drop a setting.
func ParseConfig(raw []byte) ([]Model, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(f.Models) == 0 {
		return nil, errors.New("configure at least one model")
	}
	var out []Model
	for i, fm := range f.Models {
		m := Model{
			Name: fm.Name, BaseURL: fm.BaseURL, Model: fm.Model, Jobs: fm.Jobs,
			Concurrency: 1, Local: true, ContextTokens: fm.ContextTokens, ThinkingBudget: fm.ThinkingBudget,
			RequestTimeout: DefaultRequestTimeout, APIKeyEnv: fm.APIKeyEnv,
		}
		if fm.Concurrency != nil {
			m.Concurrency = *fm.Concurrency
		}
		if fm.Local != nil {
			m.Local = *fm.Local
		}
		if fm.RequestTimeout != "" {
			d, err := time.ParseDuration(fm.RequestTimeout)
			if err != nil {
				return nil, fmt.Errorf("model %d (%s): requestTimeout: %w", i+1, m.Name, err)
			}
			m.RequestTimeout = d
		}
		if m.APIKeyEnv != "" {
			if m.apiKey = os.Getenv(m.APIKeyEnv); m.apiKey == "" {
				return nil, fmt.Errorf("model %d (%s): apiKeyEnv %s is not set", i+1, m.Name, m.APIKeyEnv)
			}
		}
		m = m.withDefaults()
		if err := m.check(out); err != nil {
			return nil, fmt.Errorf("model %d (%s): %w", i+1, m.Name, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Default returns m, named DefaultName and authenticated with apiKey, as
// the only model: the one the LLM_* variables configure when there is no
// models file.
func Default(m Model, apiKey string) ([]Model, error) {
	m.Name, m.Local, m.apiKey = DefaultName, true, apiKey
	if m.RequestTimeout == 0 {
		m.RequestTimeout = DefaultRequestTimeout
	}
	m = m.withDefaults()
	if err := m.check(nil); err != nil {
		return nil, err
	}
	return []Model{m}, nil
}

// withDefaults gives a model serving no job in particular every job.
func (m Model) withDefaults() Model {
	if len(m.Jobs) == 0 {
		m.Jobs = slices.Clone(Jobs)
	}
	return m
}

// check validates m, given the models before it.
func (m Model) check(before []Model) error {
	if !namePattern.MatchString(m.Name) {
		return fmt.Errorf("name %q: want a lowercase letter, then up to 15 lowercase letters or digits", m.Name)
	}
	if slices.ContainsFunc(before, func(o Model) bool { return o.Name == m.Name }) {
		return errors.New("duplicate name")
	}
	if m.BaseURL != "" {
		u, err := url.Parse(m.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("baseURL %q: want an absolute http or https URL", m.BaseURL)
		}
		if m.Model == "" {
			return errors.New("model is required with a baseURL")
		}
	}
	if m.Concurrency <= 0 {
		return errors.New("concurrency must be positive")
	}
	for _, j := range m.Jobs {
		if !slices.Contains(Jobs, j) {
			return fmt.Errorf("job %q: want one of %v", j, Jobs)
		}
	}
	if m.ContextTokens < 0 {
		return errors.New("contextTokens must not be negative")
	}
	if m.ThinkingBudget < 0 {
		return errors.New("thinkingBudget must not be negative")
	}
	if m.RequestTimeout <= 0 {
		return errors.New("requestTimeout must be positive")
	}
	return nil
}
