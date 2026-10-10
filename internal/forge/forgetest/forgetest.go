// Package forgetest provides an in-memory forge for tests.
package forgetest

import (
	"context"
	"errors"
	"sync"

	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
)

// Fake is an in-memory forge.Forge.
type Fake struct {
	ForgeName string
	URL       string
	Login     string
	Remote    gitrepo.Remote

	mu  sync.Mutex
	prs map[int]forge.PR
	// Reviews holds submitted and pending reviews.
	reviews []forge.Review
	// RejectInline fails reviews that carry inline comments, like GitHub
	// does for a line outside the diff.
	RejectInline bool
	// RejectVerdicts fails approvals and change requests, like a forge
	// where the bot may only comment.
	RejectVerdicts bool
	created        []forge.NewReview
}

// New returns a fake forge.
func New(name, url string) *Fake {
	return &Fake{ForgeName: name, URL: url, Login: "agent-nexus", prs: map[int]forge.PR{}}
}

// SetPR adds or replaces a pull request.
func (f *Fake) SetPR(pr forge.PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[pr.Number] = pr
}

// AddReview adds an existing review.
func (f *Fake) AddReview(r forge.Review) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviews = append(f.reviews, r)
}

// Created returns the reviews created so far.
func (f *Fake) Created() []forge.NewReview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.NewReview(nil), f.created...)
}

// Name implements forge.Forge.
func (f *Fake) Name() string { return f.ForgeName }

// ServerURL implements forge.Forge.
func (f *Fake) ServerURL() string { return f.URL }

// BotLogin implements forge.Forge.
func (f *Fake) BotLogin(context.Context) (string, error) { return f.Login, nil }

// GetPR implements forge.Forge.
func (f *Fake) GetPR(_ context.Context, _ forge.Repo, n int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.prs[n]
	if !ok {
		return forge.PR{}, forge.ErrNotFound
	}
	return pr, nil
}

// ListReviews implements forge.Forge.
func (f *Fake) ListReviews(context.Context, forge.Repo, int) ([]forge.Review, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Review(nil), f.reviews...), nil
}

// DeletePendingReviews implements forge.Forge.
func (f *Fake) DeletePendingReviews(context.Context, forge.Repo, int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.reviews[:0]
	for _, r := range f.reviews {
		if r.State != "PENDING" || r.User != f.Login {
			kept = append(kept, r)
		}
	}
	f.reviews = kept
	return nil
}

// CreateReview implements forge.Forge.
func (f *Fake) CreateReview(_ context.Context, _ forge.Repo, _ int, r forge.NewReview) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RejectInline && len(r.Comments) > 0 {
		return errors.New("422 line could not be resolved")
	}
	if f.RejectVerdicts && r.Event != "" && r.Event != forge.EventComment {
		return errors.New("403 the bot may not approve or request changes")
	}
	f.created = append(f.created, r)
	f.reviews = append(f.reviews, forge.Review{ID: int64(len(f.reviews) + 1), User: f.Login, Body: r.Body, State: "COMMENT", CommitID: r.CommitSHA})
	return nil
}

// GitRemote implements forge.Forge.
func (f *Fake) GitRemote(context.Context, forge.Repo) (gitrepo.Remote, error) { return f.Remote, nil }
