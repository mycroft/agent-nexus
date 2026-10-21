package main

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/mycroft/agent-nexus/internal/config"
	"github.com/mycroft/agent-nexus/internal/models"
	"github.com/mycroft/agent-nexus/internal/review"
	"github.com/mycroft/agent-nexus/internal/zulip"
)

func parseModels(t *testing.T, raw string) []models.Model {
	t.Helper()
	ms, err := models.ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestJobModels(t *testing.T) {
	ms := parseModels(t, `
models:
  - name: big
    baseURL: http://llm/v1
    model: big-model
    contextTokens: 32768
    concurrency: 2
    jobs: [review, discussion]
  - name: small
    baseURL: http://llm/v1
    model: small-model
    contextTokens: 8192
    jobs: [chat, discussion]
  - name: gpu
    jobs: [review]
`)
	a := &app{cfg: config.Config{Models: ms}, log: slog.New(slog.DiscardHandler), reviewer: &review.Reviewer{}}

	// Without Zulip, no model serves chat.
	got, err := a.jobModels(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("models = %+v", got)
	}
	big, small, gpu := got[0], got[1], got[2]
	if big.Name != "big" || big.Concurrency != 2 || !slices.Equal(big.Kinds, []string{review.Kind, review.DiscussionKind}) || big.Handlers == nil {
		t.Errorf("big = %+v", big)
	}
	if !slices.Equal(small.Kinds, []string{review.DiscussionKind}) {
		t.Errorf("small kinds = %v, want discussion only", small.Kinds)
	}
	// A model without an endpoint only takes jobs from runners.
	if gpu.Handlers != nil || !slices.Equal(gpu.Kinds, []string{review.Kind}) {
		t.Errorf("gpu = %+v", gpu)
	}

	// Each local model answers with its own copy of the reviewer.
	hs, err := big.Handlers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := hs[review.Kind].(*review.Reviewer)
	if !ok || r.Model != "big-model" || r.ContextTokens != 32768 || r == a.reviewer || len(hs) != 2 {
		t.Errorf("big handlers = %+v", hs)
	}

	withZulip, err := a.jobModels(zulip.New("https://zulip.example.com", "bot@example.com", "key"))
	if err != nil {
		t.Fatal(err)
	}
	hs, err = withZulip[1].Handlers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := hs[review.ChatKind].(*review.Chat); !ok || c.Reviewer.Model != "small-model" {
		t.Errorf("small handlers = %+v", hs)
	}

	// Every kind must be served by some model.
	a.cfg.Models = parseModels(t, "models:\n  - name: a\n    baseURL: http://llm/v1\n    model: m\n    jobs: [review]\n")
	if _, err := a.jobModels(nil); err == nil {
		t.Error("jobModels accepted a configuration serving no discussion")
	}
}

func TestReviewModel(t *testing.T) {
	ms := parseModels(t, `
models:
  - name: gpu
    jobs: [review]
  - name: chat
    baseURL: http://llm/v1
    model: m
    jobs: [chat]
  - name: big
    baseURL: http://llm/v1
    model: m
`)
	if m, err := reviewModel(ms, ""); err != nil || m.Name != "big" {
		t.Errorf("default = %+v, %v; want the first runnable model serving reviews", m.Name, err)
	}
	if m, err := reviewModel(ms, "chat"); err != nil || m.Name != "chat" {
		t.Errorf("named = %+v, %v", m.Name, err)
	}
	for _, name := range []string{"gpu", "missing"} {
		if _, err := reviewModel(ms, name); err == nil {
			t.Errorf("reviewModel(%q) succeeded", name)
		}
	}
	if _, err := reviewModel(ms[:2], ""); err == nil {
		t.Error("reviewModel found a model without one serving reviews")
	}
}
