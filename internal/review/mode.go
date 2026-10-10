package review

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/mycroft/agent-nexus/internal/forge"
)

// Mode decides how a review is submitted.
type Mode string

// Review modes.
const (
	// ModeComment posts a plain comment review without a verdict.
	ModeComment Mode = "comment"
	// ModeVerdict submits the verdict: approve, comment or request changes.
	ModeVerdict Mode = "verdict"
)

// RepoConfigFile is read from the base branch of reviewed repositories.
const RepoConfigFile = ".agent-nexus.yaml"

func parseMode(s string) (Mode, error) {
	switch m := Mode(strings.TrimSpace(s)); m {
	case ModeComment, ModeVerdict:
		return m, nil
	default:
		return "", fmt.Errorf("unknown review mode %q, want %q or %q", s, ModeComment, ModeVerdict)
	}
}

// ModeRule maps repositories ("owner/name", or a path.Match pattern such
// as "mycroft/*") to a mode.
type ModeRule struct {
	Pattern string
	Mode    Mode
}

func (r ModeRule) exact() bool { return !strings.ContainsAny(r.Pattern, `*?[\`) }

// ParseModeRules parses "pattern=mode,pattern=mode".
func ParseModeRules(s string) ([]ModeRule, error) {
	var rules []ModeRule
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		pattern, value, ok := strings.Cut(entry, "=")
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if !ok || !strings.Contains(pattern, "/") {
			return nil, fmt.Errorf("review mode rule %q: want owner/name=mode", entry)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return nil, fmt.Errorf("review mode rule %q: %w", entry, err)
		}
		mode, err := parseMode(value)
		if err != nil {
			return nil, fmt.Errorf("review mode rule %q: %w", entry, err)
		}
		rules = append(rules, ModeRule{Pattern: pattern, Mode: mode})
	}
	return rules, nil
}

// repoConfig is the content of RepoConfigFile.
type repoConfig struct {
	Review struct {
		Mode string `yaml:"mode"`
	} `yaml:"review"`
}

// parseRepoConfig returns the mode set by a repository's config file, or
// "" when it sets none.
func parseRepoConfig(content []byte) (Mode, error) {
	var cfg repoConfig
	dec := yaml.NewDecoder(bytes.NewReader(content))
	dec.KnownFields(true)
	// An empty file decodes to io.EOF: it sets nothing.
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("parse %s: %w", RepoConfigFile, err)
	}
	if cfg.Review.Mode == "" {
		return "", nil
	}
	mode, err := parseMode(cfg.Review.Mode)
	if err != nil {
		return "", fmt.Errorf("%s: review.mode: %w", RepoConfigFile, err)
	}
	return mode, nil
}

// resolveMode picks the mode for repo, first match wins: a central rule
// naming the repository exactly, then the repository's own config file,
// then central patterns, then comment. It also says which one decided.
func resolveMode(rules []ModeRule, repo forge.Repo, fileMode Mode) (Mode, string) {
	name := strings.ToLower(repo.String())
	for _, r := range rules {
		if r.exact() && r.Pattern == name {
			return r.Mode, "central rule " + r.Pattern
		}
	}
	if fileMode != "" {
		return fileMode, RepoConfigFile
	}
	for _, r := range rules {
		if ok, _ := path.Match(r.Pattern, name); ok && !r.exact() {
			return r.Mode, "central rule " + r.Pattern
		}
	}
	return ModeComment, "default"
}

// verdictEvent turns the model's verdict into a review event, downgrading
// a verdict that contradicts the findings to a comment: approving needs no
// blocking or important finding, requesting changes needs a blocking one.
// The note explains a downgrade.
func verdictEvent(out Output) (forge.Event, string) {
	blocking, important := false, false
	for _, f := range out.Findings {
		blocking = blocking || f.Severity == "blocking"
		important = important || f.Severity == "important"
	}
	switch out.Verdict {
	case "approve":
		if blocking || important {
			return forge.EventComment, "Approval withheld: the review has blocking or important findings."
		}
		return forge.EventApprove, ""
	case "request_changes":
		if !blocking {
			return forge.EventComment, "Change request withheld: the review has no blocking finding."
		}
		return forge.EventRequestChanges, ""
	default:
		return forge.EventComment, ""
	}
}
