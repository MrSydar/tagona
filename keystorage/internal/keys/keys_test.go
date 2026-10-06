package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	raw, hash, keyPrefix, err := Generate()
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	if !strings.HasPrefix(raw, Prefix) {
		t.Errorf("raw key should start with %q, got %q", Prefix, raw)
	}
	hexPart := strings.TrimPrefix(raw, Prefix)
	if len(hexPart) != 64 {
		t.Errorf("raw key should have 64 hex chars after prefix, got %d (%q)", len(hexPart), raw)
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		t.Errorf("raw key suffix should be lowercase hex: %v", err)
	}
	if strings.ToLower(hexPart) != hexPart {
		t.Errorf("raw key suffix should be lowercase hex, got %q", hexPart)
	}
	sum := sha256.Sum256([]byte(raw))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Errorf("hash should be sha256 hex of raw key, got %q want %q", hash, want)
	}
	if len(hash) != 64 {
		t.Errorf("hash should be 64 hex chars, got %d", len(hash))
	}
	if keyPrefix != raw[:12] {
		t.Errorf("keyPrefix should be first 12 chars of raw key, got %q want %q", keyPrefix, raw[:12])
	}
	if !strings.HasPrefix(raw, keyPrefix) {
		t.Errorf("keyPrefix %q should be a prefix of raw %q", keyPrefix, raw)
	}
}

func TestGenerateUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		raw, _, _, err := Generate()
		if err != nil {
			t.Fatalf("Generate returned error: %v", err)
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("duplicate key generated at iteration %d: %s", i, raw)
		}
		seen[raw] = struct{}{}
	}
}

func TestHash(t *testing.T) {
	raw := Prefix + "0123456789abcdef"
	sum := sha256.Sum256([]byte(raw))
	if want := hex.EncodeToString(sum[:]); Hash(raw) != want {
		t.Errorf("Hash(%q) = %q, want %q", raw, Hash(raw), want)
	}
}
