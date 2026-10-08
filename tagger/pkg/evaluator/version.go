package evaluator

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// versionOf joins an implementation name and its model into a version: "<implementation>:<model>", or
// just the implementation when there is no model.
func versionOf(implementation, model string) string {
	if model == "" {
		return implementation
	}
	return implementation + ":" + model
}

// MaxVersionBytes is the longest version a collection can record.
const MaxVersionBytes = 128

// ValidateVersion checks a version that was set by hand: 1 to 128 bytes of valid UTF-8 without control
// characters.
func ValidateVersion(v string) error {
	if v == "" || len(v) > MaxVersionBytes {
		return fmt.Errorf("must be 1 to %d bytes", MaxVersionBytes)
	}
	if !utf8.ValidString(v) {
		return fmt.Errorf("must be valid UTF-8")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("must not contain control characters")
		}
	}
	return nil
}
