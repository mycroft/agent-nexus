// Package mcp gives the agent the tools of MCP servers: Kubernetes,
// Grafana, or any other backend whose credentials stay in its own server.
// It speaks MCP through the official Go SDK, which handles both protocol
// eras (server/discover, and initialize for older servers).
package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

// The jobs that may get a server's tools.
const (
	JobChat       = "chat"
	JobReview     = "review"
	JobDiscussion = "discussion"
)

var jobs = []string{JobChat, JobReview, JobDiscussion}

// How a job gets a server's tools.
const (
	// LoadLazy lists the server in the prompt; the model loads its tools
	// through mcp_tools and calls them through mcp_call, and the server is
	// reached only then.
	LoadLazy = "lazy"
	// LoadEager opens the server when the job starts and offers its tools
	// as the model's own: k8s_pods_list.
	LoadEager = "eager"
)

var loads = []string{LoadLazy, LoadEager}

// DefaultTimeout bounds one tool call when the configuration sets none.
const DefaultTimeout = time.Minute

// namePattern is that of a server name.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// reservedName prefixes the tools loading lazy servers: an eager server
// of that name could collide with them.
const reservedName = "mcp"

// Server configures one MCP server.
type Server struct {
	// Name names the server to the model, and prefixes its tools when it
	// is eager: k8s_pods_list.
	Name string
	// URL is the server's Streamable HTTP endpoint.
	URL string
	// Description tells the model what the server reads.
	Description string
	// Tools are path.Match patterns of the tools the model may call.
	Tools []string
	// Jobs are the jobs that get the server's tools.
	Jobs []string
	// Load is LoadLazy or LoadEager; "" is lazy.
	Load string
	// Timeout bounds one tool call.
	Timeout time.Duration
	// TokenEnv names the environment variable holding a bearer token for
	// the server.
	TokenEnv string

	token string
}

// Serves reports whether job gets the server's tools.
func (s Server) Serves(job string) bool { return slices.Contains(s.Jobs, job) }

// Lazy reports whether the model loads the server's tools itself, when
// it needs them.
func (s Server) Lazy() bool { return s.Load != LoadEager }

// allowed reports whether the model may call the tool name.
func (s Server) allowed(name string) bool {
	return slices.ContainsFunc(s.Tools, func(p string) bool {
		ok, _ := path.Match(p, name)
		return ok
	})
}

// file is the configuration file's layout.
type file struct {
	Servers []struct {
		Name        string   `yaml:"name"`
		URL         string   `yaml:"url"`
		Description string   `yaml:"description"`
		Tools       []string `yaml:"tools"`
		Jobs        []string `yaml:"jobs"`
		Load        string   `yaml:"load"`
		Timeout     string   `yaml:"timeout"`
		TokenEnv    string   `yaml:"tokenEnv"`
	} `yaml:"servers"`
}

// LoadConfig reads the servers configured in the YAML file at p. An empty
// path configures none.
func LoadConfig(p string) ([]Server, error) {
	if p == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("mcp config: %w", err)
	}
	servers, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("mcp config %s: %w", p, err)
	}
	return servers, nil
}

// ParseConfig parses and checks a configuration file. Unknown keys are
// errors, so a typo does not silently drop a setting.
func ParseConfig(raw []byte) ([]Server, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var out []Server
	for i, fs := range f.Servers {
		s := Server{
			Name: fs.Name, URL: fs.URL, Description: fs.Description,
			Tools: fs.Tools, Jobs: fs.Jobs, Load: fs.Load, TokenEnv: fs.TokenEnv, Timeout: DefaultTimeout,
		}
		if len(s.Jobs) == 0 {
			s.Jobs = []string{JobChat}
		}
		if s.Load == "" {
			s.Load = LoadLazy
		}
		if fs.Timeout != "" {
			d, err := time.ParseDuration(fs.Timeout)
			if err != nil {
				return nil, fmt.Errorf("server %d (%s): timeout: %w", i+1, s.Name, err)
			}
			s.Timeout = d
		}
		if err := s.check(out); err != nil {
			return nil, fmt.Errorf("server %d (%s): %w", i+1, s.Name, err)
		}
		if s.TokenEnv != "" {
			if s.token = os.Getenv(s.TokenEnv); s.token == "" {
				return nil, fmt.Errorf("server %d (%s): tokenEnv %s is not set", i+1, s.Name, s.TokenEnv)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// check validates s, given the servers before it.
func (s Server) check(before []Server) error {
	if !namePattern.MatchString(s.Name) {
		return fmt.Errorf("name %q: want a lowercase letter, then up to 15 lowercase letters or digits", s.Name)
	}
	if s.Name == reservedName {
		return fmt.Errorf("name %q is reserved", s.Name)
	}
	if slices.ContainsFunc(before, func(o Server) bool { return o.Name == s.Name }) {
		return errors.New("duplicate name")
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q: want an absolute http or https URL", s.URL)
	}
	if len(s.Tools) == 0 {
		return errors.New("tools is required: list the tools the model may call (patterns, as path.Match; \"*\" allows all)")
	}
	for _, p := range s.Tools {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("tools pattern %q: %w", p, err)
		}
	}
	for _, j := range s.Jobs {
		if !slices.Contains(jobs, j) {
			return fmt.Errorf("job %q: want one of %v", j, jobs)
		}
	}
	if !slices.Contains(loads, s.Load) {
		return fmt.Errorf("load %q: want one of %v", s.Load, loads)
	}
	if s.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}
