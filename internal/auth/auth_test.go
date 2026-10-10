package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var (
	keyA = strings.Repeat("a", 32)
	keyB = strings.Repeat("b", 40)
)

func TestParseKeys(t *testing.T) {
	if _, err := ParseKeys("gitea="+keyA+", github="+keyB, 10); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", " , ", "noequals", "=" + keyA, "x=short", "x=" + keyA + ",x=" + keyB} {
		if _, err := ParseKeys(bad, 10); err == nil {
			t.Errorf("ParseKeys(%q) accepted", bad)
		}
	}
}

func TestMiddleware(t *testing.T) {
	keys, err := ParseKeys("gitea="+keyA+",github="+keyB, 2)
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	h := keys.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = KeyName(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	do := func(header string) int {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, header := range []string{"", "Bearer", "Bearer wrong", "Basic " + keyA, "Bearer " + keyA + "x", keyA} {
		if code := do(header); code != http.StatusUnauthorized {
			t.Errorf("Authorization %q → %d, want 401", header, code)
		}
	}
	if code := do("Bearer " + keyB); code != http.StatusNoContent || seen != "github" {
		t.Errorf("valid key → %d, name %q", code, seen)
	}
	// Burst of 2 per key: the github key has one left, then is limited;
	// the gitea key has its own bucket.
	if code := do("Bearer " + keyB); code != http.StatusNoContent {
		t.Errorf("second request → %d, want 204", code)
	}
	if code := do("Bearer " + keyB); code != http.StatusTooManyRequests {
		t.Errorf("third request → %d, want 429", code)
	}
	if code := do("Bearer " + keyA); code != http.StatusNoContent || seen != "gitea" {
		t.Errorf("other key → %d, name %q", code, seen)
	}
}
