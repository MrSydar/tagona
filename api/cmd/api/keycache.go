package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	keyCacheMaxEntries = 10000
	// negativeKeyTTL bounds how long an unknown key is remembered, so garbage
	// keys don't hit storage on every request while a freshly minted key is
	// never rejected for long.
	negativeKeyTTL = 5 * time.Second
)

// keyValidator reports whether a raw API key is valid. *keystorageapi.Client
// implements it.
type keyValidator interface {
	ValidateKey(ctx context.Context, rawKey string) (bool, error)
}

type cachedVerdict struct {
	valid   bool
	expires time.Time
}

// cachedValidator memoizes key validation verdicts for a short TTL so the
// gateway does not make a storage round trip (and DB lookup) per request.
// Raw keys are never stored, only their SHA-256. Transport errors are not
// cached.
type cachedValidator struct {
	next keyValidator
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]cachedVerdict
}

func newCachedValidator(next keyValidator, ttl time.Duration) *cachedValidator {
	return &cachedValidator{next: next, ttl: ttl, now: time.Now, entries: make(map[string]cachedVerdict)}
}

func keyDigest(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

func (c *cachedValidator) ValidateKey(ctx context.Context, rawKey string) (bool, error) {
	digest := keyDigest(rawKey)
	now := c.now()

	c.mu.Lock()
	if e, ok := c.entries[digest]; ok && now.Before(e.expires) {
		c.mu.Unlock()
		return e.valid, nil
	}
	c.mu.Unlock()

	valid, err := c.next.ValidateKey(ctx, rawKey)
	if err != nil {
		return false, err
	}

	ttl := c.ttl
	if !valid && ttl > negativeKeyTTL {
		ttl = negativeKeyTTL
	}
	c.mu.Lock()
	if len(c.entries) >= keyCacheMaxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= keyCacheMaxEntries {
			c.entries = make(map[string]cachedVerdict)
		}
	}
	c.entries[digest] = cachedVerdict{valid: valid, expires: now.Add(ttl)}
	c.mu.Unlock()
	return valid, nil
}

// Purge drops every cached verdict. Called after a key is deleted through the
// gateway so revocation takes effect immediately rather than after the TTL.
func (c *cachedValidator) Purge() {
	c.mu.Lock()
	c.entries = make(map[string]cachedVerdict)
	c.mu.Unlock()
}
