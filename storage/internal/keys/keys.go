package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// Prefix is the human-recognizable prefix of every raw API key.
const Prefix = "tagona_"

// Generate creates a new API key. It returns the raw key ("tagona_" + 64 hex
// chars from 32 random bytes), its SHA-256 hex hash for storage, and its
// display prefix (the first 12 chars of the raw key).
func Generate() (raw, hash, keyPrefix string, err error) {
	slog.Debug("Generate: called")
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("read random bytes: %w", err)
	}
	raw = Prefix + hex.EncodeToString(b)
	hash = Hash(raw)
	keyPrefix = raw[:12]
	return raw, hash, keyPrefix, nil
}

// Hash returns the SHA-256 hex hash of a raw API key.
func Hash(raw string) string {
	slog.Debug("Hash: called")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
