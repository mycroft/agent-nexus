package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/httpclient"
)

var fastRetry = httpclient.WithRetry(0, 0, 0)

func TestParseRepo(t *testing.T) {
	for _, ok := range []string{"mycroft/k8s-home", "a.b/c_d"} {
		if _, err := ParseRepo(ok); err != nil {
			t.Errorf("ParseRepo(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "x", "a/b/c", "../x", "a/..", "a/b?c", "a /b"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("ParseRepo(%q) accepted", bad)
		}
	}
}

func TestGitea(t *testing.T) {
	var created map[string]any
	var deleted []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"login":"agent-nexus"}`)
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/3", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"number":3,"title":"T","body":"B","user":{"login":"me"},"state":"open","draft":true,
			"html_url":"https://g/o/r/pulls/3","base":{"ref":"main","sha":"b"},"head":{"sha":"h"}}`)
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/3/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[
			{"id":1,"user":{"login":"agent-nexus"},"state":"PENDING","body":""},
			{"id":2,"user":{"login":"someone"},"state":"PENDING","body":""},
			{"id":3,"user":{"login":"agent-nexus"},"state":"COMMENT","body":"done"}]`)
	})
	mux.HandleFunc("DELETE /api/v1/repos/o/r/pulls/3/reviews/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleted = append(deleted, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/repos/o/r/pulls/3/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&created)
		_, _ = io.WriteString(w, `{"id":4}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGitea(srv.URL+"/", "agent-nexus", "tok", fastRetry)
	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}

	pr, err := g.GetPR(ctx, repo, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := PR{Number: 3, Title: "T", Body: "B", Author: "me", State: "open", Draft: true, BaseRef: "main", HeadSHA: "h", URL: "https://g/o/r/pulls/3"}
	if pr != want {
		t.Errorf("GetPR = %+v, want %+v", pr, want)
	}
	if _, err := g.GetPR(ctx, repo, 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPR(missing) error = %v, want ErrNotFound", err)
	}

	if err := g.DeletePendingReviews(ctx, repo, 3); err != nil {
		t.Fatal(err)
	}
	if strings.Join(deleted, ",") != "1" {
		t.Errorf("deleted reviews %v, want only the bot's pending review 1", deleted)
	}

	err = g.CreateReview(ctx, repo, 3, NewReview{CommitSHA: "h", Body: "body", Comments: []Comment{{Path: "a.go", Line: 12, Body: "c"}}})
	if err != nil {
		t.Fatal(err)
	}
	comments, _ := created["comments"].([]any)
	first, _ := comments[0].(map[string]any)
	if created["event"] != "COMMENT" || created["commit_id"] != "h" || first["new_position"] != float64(12) || first["path"] != "a.go" {
		t.Errorf("create review body = %v", created)
	}
	for event, want := range map[Event]string{EventApprove: "APPROVED", EventRequestChanges: "REQUEST_CHANGES", EventComment: "COMMENT"} {
		if err := g.CreateReview(ctx, repo, 3, NewReview{CommitSHA: "h", Event: event, Body: "b"}); err != nil {
			t.Fatal(err)
		}
		if created["event"] != want {
			t.Errorf("event %s sent as %v, want %s", event, created["event"], want)
		}
	}

	remote, err := g.GitRemote(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if remote.URL != srv.URL+"/o/r.git" || remote.Key != "gitea/o/r" || !strings.HasPrefix(remote.AuthHeader, "Basic ") {
		t.Errorf("GitRemote = %+v", remote)
	}
}

func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return key, string(pemKey)
}

func verifyJWT(t *testing.T, pub *rsa.PublicKey, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed JWT %q", token)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("JWT signature invalid: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestGitHubAppAuth(t *testing.T) {
	key, pemKey := testKey(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clock := now
	var installCalls, tokenCalls atomic.Int32
	var review map[string]any

	mux := http.NewServeMux()
	appOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			claims := verifyJWT(t, &key.PublicKey, token)
			if claims["iss"] != "42" || claims["iat"] != float64(clock.Add(-time.Minute).Unix()) || claims["exp"] != float64(clock.Add(9*time.Minute).Unix()) {
				t.Errorf("unexpected JWT claims %v", claims)
			}
			h(w, r)
		}
	}
	installation := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer inst-token" || r.Header.Get("X-Github-Api-Version") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /app", appOnly(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"slug":"agent-nexus"}`)
	}))
	mux.HandleFunc("GET /repos/o/r/installation", appOnly(func(w http.ResponseWriter, _ *http.Request) {
		installCalls.Add(1)
		_, _ = io.WriteString(w, `{"id":99}`)
	}))
	mux.HandleFunc("POST /app/installations/99/access_tokens", appOnly(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		_, _ = io.WriteString(w, `{"token":"inst-token","expires_at":"`+now.Add(time.Hour).Format(time.RFC3339)+`"}`)
	}))
	mux.HandleFunc("GET /repos/o/r/pulls/5", installation(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"number":5,"title":"T","user":{"login":"me"},"state":"open","draft":false,
			"base":{"ref":"main"},"head":{"sha":"h"}}`)
	}))
	mux.HandleFunc("POST /repos/o/r/pulls/5/reviews", installation(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&review)
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g, err := NewGitHub(srv.URL, "https://github.com", 42, pemKey, fastRetry)
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return clock }
	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}

	login, err := g.BotLogin(ctx)
	if err != nil || login != "agent-nexus[bot]" {
		t.Fatalf("BotLogin = %q, %v", login, err)
	}
	for range 3 {
		if _, err := g.GetPR(ctx, repo, 5); err != nil {
			t.Fatal(err)
		}
	}
	if installCalls.Load() != 1 || tokenCalls.Load() != 1 {
		t.Errorf("installation lookups = %d, token mints = %d; want 1 and 1 (cached)", installCalls.Load(), tokenCalls.Load())
	}

	// Within the refresh margin of expiry, a new token is minted.
	clock = now.Add(56 * time.Minute)
	if _, err := g.installationToken(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 2 {
		t.Errorf("token mints = %d after nearing expiry, want 2", tokenCalls.Load())
	}
	clock = now

	err = g.CreateReview(ctx, repo, 5, NewReview{CommitSHA: "h", Body: "b", Comments: []Comment{{Path: "a.go", Line: 3, Body: "c"}}})
	if err != nil {
		t.Fatal(err)
	}
	comments, _ := review["comments"].([]any)
	first, _ := comments[0].(map[string]any)
	if review["event"] != "COMMENT" || first["side"] != "RIGHT" || first["line"] != float64(3) {
		t.Errorf("review body = %v", review)
	}
	for event, want := range map[Event]string{EventApprove: "APPROVE", EventRequestChanges: "REQUEST_CHANGES"} {
		if err := g.CreateReview(ctx, repo, 5, NewReview{CommitSHA: "h", Event: event, Body: "b"}); err != nil {
			t.Fatal(err)
		}
		if review["event"] != want {
			t.Errorf("event %s sent as %v, want %s", event, review["event"], want)
		}
	}

	remote, err := g.GitRemote(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:inst-token"))
	if remote.URL != "https://github.com/o/r.git" || remote.AuthHeader != wantAuth {
		t.Errorf("GitRemote = %+v", remote)
	}
}

func TestGitHubRejectsBadKey(t *testing.T) {
	if _, err := NewGitHub("https://api.github.com", "https://github.com", 1, "not a key"); err == nil {
		t.Error("NewGitHub accepted a non-PEM key")
	}
}

func TestGiteaNotes(t *testing.T) {
	var replied, commented map[string]any
	var repliedPath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/3/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[
			{"id":1,"user":{"login":"agent-nexus"},"state":"COMMENT","body":"## Summary","comments_count":1,"submitted_at":"2026-01-01T00:00:01Z"},
			{"id":2,"user":{"login":"agent-nexus"},"state":"PENDING","body":"draft","comments_count":1,"submitted_at":"2026-01-01T00:00:02Z"},
			{"id":3,"user":{"login":"alice"},"state":"APPROVED","body":"","comments_count":0,"submitted_at":"2026-01-01T00:00:09Z"}]`)
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/3/reviews/1/comments", func(w http.ResponseWriter, _ *http.Request) {
		// A finding, the reply stored in the same review on the same
		// line, and a finding on a removed line.
		_, _ = io.WriteString(w, `[
			{"id":10,"user":{"login":"agent-nexus"},"body":"finding","path":"a.go","position":12,"original_position":0,"created_at":"2026-01-01T00:00:01Z"},
			{"id":12,"user":{"login":"alice"},"body":"reply","path":"a.go","position":12,"original_position":0,"created_at":"2026-01-01T00:00:05Z"},
			{"id":11,"user":{"login":"agent-nexus"},"body":"old side","path":"a.go","position":0,"original_position":12,"created_at":"2026-01-01T00:00:01Z"}]`)
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/3/reviews/2/comments", func(http.ResponseWriter, *http.Request) {
		t.Error("comments of a pending review were fetched")
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/issues/3/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":20,"user":{"login":"bob"},"body":"@agent-nexus hi","created_at":"2026-01-01T00:00:03Z"}]`)
	})
	mux.HandleFunc("POST /api/v1/repos/o/r/pulls/3/comments/{id}/replies", func(w http.ResponseWriter, r *http.Request) {
		repliedPath = r.PathValue("id")
		_ = json.NewDecoder(r.Body).Decode(&replied)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /api/v1/repos/o/r/issues/3/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&commented)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := NewGitea(srv.URL, "agent-nexus", "tok", fastRetry)
	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}
	notes, err := g.ListNotes(ctx, repo, 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range notes {
		got = append(got, fmt.Sprintf("%s/%s/%s/%d", n.Key(), n.User, n.Thread, n.Line))
	}
	want := "review:1/agent-nexus//0,inline:10/agent-nexus/1:a.go:12/12,inline:11/agent-nexus/1:a.go:-12/0,comment:20/bob//0,inline:12/alice/1:a.go:12/12"
	if strings.Join(got, ",") != want {
		t.Errorf("notes =\n%s\nwant\n%s", strings.Join(got, ","), want)
	}

	if err := g.ReplyInline(ctx, repo, 3, notes[1], "answer"); err != nil {
		t.Fatal(err)
	}
	if repliedPath != "10" || replied["body"] != "answer" {
		t.Errorf("reply to %s with %v", repliedPath, replied)
	}
	if err := g.Comment(ctx, repo, 3, "hello"); err != nil {
		t.Fatal(err)
	}
	if commented["body"] != "hello" {
		t.Errorf("comment = %v", commented)
	}
}

func TestGitHubNotes(t *testing.T) {
	_, pemKey := testKey(t)
	var repliedTo string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/installation", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":99}`)
	})
	mux.HandleFunc("POST /app/installations/99/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"token":"inst","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/5/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":1,"user":{"login":"agent-nexus[bot]"},"state":"COMMENTED","body":"summary","submitted_at":"2026-01-01T00:00:01Z"}]`)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/5/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[
			{"id":10,"user":{"login":"agent-nexus[bot]"},"body":"finding","path":"a.go","line":3,"created_at":"2026-01-01T00:00:01Z"},
			{"id":11,"user":{"login":"alice"},"body":"reply","path":"a.go","line":3,"in_reply_to_id":10,"created_at":"2026-01-01T00:00:02Z"}]`)
	})
	mux.HandleFunc("GET /repos/o/r/issues/5/comments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("POST /repos/o/r/pulls/5/comments/{id}/replies", func(w http.ResponseWriter, r *http.Request) {
		repliedTo = r.PathValue("id")
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g, err := NewGitHub(srv.URL, "https://github.com", 42, pemKey, fastRetry)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := Repo{Owner: "o", Name: "r"}
	notes, err := g.ListNotes(ctx, repo, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 3 || notes[1].Thread != "10" || notes[2].Thread != "10" || notes[2].User != "alice" {
		t.Fatalf("notes = %+v", notes)
	}
	if err := g.ReplyInline(ctx, repo, 5, notes[1], "answer"); err != nil || repliedTo != "10" {
		t.Errorf("reply to %q: %v", repliedTo, err)
	}
}
