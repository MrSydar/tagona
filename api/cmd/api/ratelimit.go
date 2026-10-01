package main

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const limiterIdleTTL = 10 * time.Minute

type bucket struct {
	tokens float64
	last   time.Time
}

// keyRateLimiter is a per-API-key token bucket. It must run after
// apiKeyAuth, so only valid keys get an entry and the map stays bounded by the
// number of issued keys.
type keyRateLimiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

func newKeyRateLimiter(rps float64, burst int) *keyRateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &keyRateLimiter{rate: rps, burst: float64(burst), now: time.Now, buckets: make(map[string]*bucket)}
}

// allow takes one token for the key, reporting whether the request may pass.
func (l *keyRateLimiter) allow(id string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > limiterIdleTTL {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[id]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// middleware returns 429 once a key exceeds its rate. A nil limiter disables
// limiting.
func (l *keyRateLimiter) middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !l.allow(keyDigest(key)) {
			slog.Debug("rate limit exceeded")
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}
