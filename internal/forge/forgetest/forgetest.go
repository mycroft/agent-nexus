// Package forgetest provides an in-memory forge for tests.
package forgetest

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/httpclient"
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
	// RejectComments fails pull request comments, like a Gitea token
	// without the write:issue scope.
	RejectComments bool
	created        []forge.NewReview
	notes          []forge.Note
	nextID         int64
	// Replies records inline replies and comments posted by the bot.
	replies []forge.Note
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

// AddNote adds a discussion note; its ID is assigned when zero.
func (f *Fake) AddNote(n forge.Note) forge.Note {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addNote(n)
}

func (f *Fake) addNote(n forge.Note) forge.Note {
	f.nextID++
	if n.ID == 0 {
		n.ID = 1000 + f.nextID
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Date(2026, 1, 1, 0, 0, int(f.nextID), 0, time.UTC)
	}
	f.notes = append(f.notes, n)
	return n
}

// Replies returns the inline replies and comments posted so far.
func (f *Fake) Replies() []forge.Note {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Note(nil), f.replies...)
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
	state := map[forge.Event]string{forge.EventApprove: "APPROVED", forge.EventRequestChanges: "REQUEST_CHANGES"}[r.Event]
	if state == "" {
		state = "COMMENT"
	}
	f.reviews = append(f.reviews, forge.Review{ID: int64(len(f.reviews) + 1), User: f.Login, Body: r.Body, State: state, CommitID: r.CommitSHA})
	f.addNote(forge.Note{Kind: forge.NoteReview, User: f.Login, Body: r.Body, Approved: state == "APPROVED"})
	return nil
}

// ListNotes implements forge.Forge.
func (f *Fake) ListNotes(context.Context, forge.Repo, int) ([]forge.Note, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.Note(nil), f.notes...), nil
}

// ReplyInline implements forge.Forge.
func (f *Fake) ReplyInline(_ context.Context, _ forge.Repo, _ int, root forge.Note, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.addNote(forge.Note{Kind: forge.NoteInline, User: f.Login, Body: body, Path: root.Path, Line: root.Line, Thread: root.Thread})
	f.replies = append(f.replies, n)
	return nil
}

// Comment implements forge.Forge.
func (f *Fake) Comment(_ context.Context, _ forge.Repo, _ int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RejectComments {
		return &httpclient.StatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: "token does not have the write:issue scope"}
	}
	n := f.addNote(forge.Note{Kind: forge.NoteComment, User: f.Login, Body: body})
	f.replies = append(f.replies, n)
	return nil
}

// GitRemote implements forge.Forge.
func (f *Fake) GitRemote(context.Context, forge.Repo) (gitrepo.Remote, error) { return f.Remote, nil }
