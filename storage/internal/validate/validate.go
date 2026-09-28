package validate

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
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

// ValidateDataType checks if data type is in the supported set.
func ValidateDataType(dataType string, supported []string) error {
	slog.Debug("ValidateDataType", "dataType", dataType, "supportedCount", len(supported))
	for _, s := range supported {
		if s == dataType {
			return nil
		}
	}
	return fmt.Errorf("unsupported data_type: %s", dataType)
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

// ValidateAPIKeyName checks if an API key name is valid.
func ValidateAPIKeyName(name string) error {
	slog.Debug("ValidateAPIKeyName", "name", name)
	if len(name) == 0 {
		return fmt.Errorf("api key name cannot be empty")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("api key name must be valid UTF-8")
	}
	if len(name) > 128 {
		// byte length check
		return fmt.Errorf("api key name exceeds 128 bytes")
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
