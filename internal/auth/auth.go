// Package auth authenticates API callers with named bearer keys and
// rate-limits each key.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// minKeyLength rejects keys too short to resist guessing.
const minKeyLength = 32

type ctxKey struct{}

type entry struct {
	name    string
	hash    [sha256.Size]byte
	limiter *rate.Limiter
}

// Keys is a set of named API keys.
type Keys struct {
	entries []entry
}

// ParseKeys parses "name=key,name=key". perMinute is the sustained request
// rate allowed per key, with a burst of the same size; zero does not
// limit it.
func ParseKeys(s string, perMinute int) (*Keys, error) {
	k := &Keys{}
	seen := map[string]bool{}
	for pair := range strings.SplitSeq(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, key, ok := strings.Cut(pair, "=")
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if !ok || name == "" {
			return nil, errors.New("API key entries must be name=key")
		}
		if len(key) < minKeyLength {
			return nil, fmt.Errorf("API key %q is shorter than %d characters", name, minKeyLength)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate API key name %q", name)
		}
		seen[name] = true
		limiter := rate.NewLimiter(rate.Inf, 0)
		if perMinute > 0 {
			limiter = rate.NewLimiter(rate.Every(time.Minute/time.Duration(perMinute)), perMinute)
		}
		k.entries = append(k.entries, entry{name: name, hash: sha256.Sum256([]byte(key)), limiter: limiter})
	}
	if len(k.entries) == 0 {
		return nil, errors.New("no API keys configured")
	}
	return k, nil
}

// authenticate returns the entry matching key. Every entry is compared,
// in constant time, so timing reveals neither the key nor which entry
// matched.
func (k *Keys) authenticate(key string) *entry {
	h := sha256.Sum256([]byte(key))
	var match *entry
	for i := range k.entries {
		if subtle.ConstantTimeCompare(h[:], k.entries[i].hash[:]) == 1 {
			match = &k.entries[i]
		}
	}
	return match
}

// Middleware rejects requests without a valid "Authorization: Bearer"
// key (401) or over the key's rate (429), and stores the key name in the
// request context.
func (k *Keys) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		e := k.authenticate(strings.TrimSpace(token))
		if !ok || e == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agent-nexus"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !e.limiter.Allow() {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, e.name)))
	})
}

// KeyName returns the authenticated key name stored by Middleware.
func KeyName(ctx context.Context) string {
	name, _ := ctx.Value(ctxKey{}).(string)
	return name
}
