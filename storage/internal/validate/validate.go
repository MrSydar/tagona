package validate

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mrsydar/tagona/storage/internal/models"
)

var collectionNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidateCollectionName checks if a collection name is valid.
func ValidateCollectionName(name string) error {
	slog.Debug("ValidateCollectionName", "name", name)
	if !collectionNameRe.MatchString(name) {
		return fmt.Errorf("collection name must be 1-64 chars, lowercase letters, digits, underscore, hyphen, starting with a letter")
	}
	return nil
}

// MaxTaggerVersionBytes is the longest tagger version a collection can record.
const MaxTaggerVersionBytes = 128

// ValidateTaggerVersion checks a tagger version: any string of 1 to 128 bytes of valid UTF-8 without
// control characters. By convention it is "<implementation>" or "<implementation>:<model>", for example
// "grep" or "decisions/openai:zai-org/GLM-5.3-Flash", but what a version means is the tagger's business.
func ValidateTaggerVersion(v string) error {
	slog.Debug("ValidateTaggerVersion", "version", v)
	if v == "" || len(v) > MaxTaggerVersionBytes {
		return fmt.Errorf("tagger_version must be 1 to %d bytes", MaxTaggerVersionBytes)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("tagger_version must be valid UTF-8")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("tagger_version must not contain control characters")
		}
	}
	return nil
}

// Limits of object metadata.
const (
	MaxMetadataEntries    = 32
	MaxMetadataKeyBytes   = 64
	MaxMetadataValueBytes = 1024
)

// ValidateMetadata checks object metadata: at most 32 entries, keys of 1 to 64 bytes and values of at most
// 1024 bytes, all valid UTF-8; keys carry no control characters and values no NUL.
func ValidateMetadata(m map[string]string) error {
	if len(m) > MaxMetadataEntries {
		return fmt.Errorf("metadata has %d entries, at most %d are allowed", len(m), MaxMetadataEntries)
	}
	for k, v := range m {
		if k == "" || len(k) > MaxMetadataKeyBytes {
			return fmt.Errorf("a metadata key must be 1 to %d bytes", MaxMetadataKeyBytes)
		}
		if !utf8.ValidString(k) {
			return fmt.Errorf("metadata key %q is not valid UTF-8", k)
		}
		for _, r := range k {
			if unicode.IsControl(r) {
				return fmt.Errorf("metadata key %q contains a control character", k)
			}
		}
		if len(v) > MaxMetadataValueBytes {
			return fmt.Errorf("the value of metadata key %q exceeds %d bytes", k, MaxMetadataValueBytes)
		}
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("the value of metadata key %q must be valid UTF-8 without NUL", k)
		}
	}
	return nil
}

// ValidateTag checks if a single tag is valid.
func ValidateTag(tag string) error {
	slog.Debug("ValidateTag", "tag", tag)
	if len(tag) == 0 {
		return fmt.Errorf("tag cannot be empty")
	}
	if !utf8.ValidString(tag) {
		return fmt.Errorf("tag must be valid UTF-8")
	}
	if len(tag) > 128 {
		// byte length check
		return fmt.Errorf("tag exceeds 128 bytes")
	}
	return nil
}

// ValidateTags checks all tags.
func ValidateTags(tags map[string]bool, maxCount int) error {
	slog.Debug("ValidateTags", "tagCount", len(tags), "maxCount", maxCount)
	if len(tags) > maxCount {
		return fmt.Errorf("too many tags in query: %d > %d", len(tags), maxCount)
	}
	for tag := range tags {
		if err := ValidateTag(tag); err != nil {
			return fmt.Errorf("invalid tag %q: %w", tag, err)
		}
	}
	return nil
}

// ValidateDateFilter checks the date filter shape.
func ValidateDateFilter(df *models.DateFilter) error {
	slog.Debug("ValidateDateFilter: called")
	if df == nil {
		return nil
	}
	if df.EQ != nil {
		if df.GT != nil || df.GTE != nil || df.LT != nil || df.LTE != nil {
			return fmt.Errorf("eq cannot be combined with other date filters")
		}
		return nil
	}
	return nil
}

// ParseDateFilterFromMap parses a date filter from a map (used when parsing JSON with raw values).
func ParseDateFilterFromMap(m map[string]string) (*models.DateFilter, error) {
	slog.Debug("ParseDateFilterFromMap", "mapSize", len(m))
	if len(m) == 0 {
		return nil, nil
	}
	df := &models.DateFilter{}
	hasEQ := false
	for k, v := range m {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("invalid date format for %s: %w", k, err)
		}
		switch strings.ToLower(k) {
		case "gt":
			df.GT = &t
		case "gte":
			df.GTE = &t
		case "lt":
			df.LT = &t
		case "lte":
			df.LTE = &t
		case "eq":
			df.EQ = &t
			hasEQ = true
		default:
			return nil, fmt.Errorf("unknown date filter key: %s", k)
		}
	}
	if hasEQ && (df.GT != nil || df.GTE != nil || df.LT != nil || df.LTE != nil) {
		return nil, fmt.Errorf("eq cannot be combined with other date filters")
	}
	return df, nil
}
