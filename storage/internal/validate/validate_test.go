package validate

import (
	"strings"
	"testing"
)

func TestValidateTaggerVersion(t *testing.T) {
	for _, ok := range []string{"grep", "false", "decisions/openai:zai-org/GLM-5.3-Flash", strings.Repeat("x", MaxTaggerVersionBytes), "模型:1"} {
		if err := ValidateTaggerVersion(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", MaxTaggerVersionBytes+1), "a\nb", "a\x00b", "\xff\xfe"} {
		if err := ValidateTaggerVersion(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestValidateMetadata(t *testing.T) {
	tooMany := map[string]string{}
	for i := 0; i < MaxMetadataEntries+1; i++ {
		tooMany[strings.Repeat("k", 1)+string(rune('a'+i%26))+strings.Repeat("0", i/26)] = "v"
	}
	good := []map[string]string{
		nil,
		{},
		{"name": "cute-dog.png"},
		{"k": ""},
		{strings.Repeat("k", MaxMetadataKeyBytes): strings.Repeat("v", MaxMetadataValueBytes)},
		{"title": "line one\nline two\ttabbed", "näme": "ünïcode ✓"},
	}
	for _, m := range good {
		if err := ValidateMetadata(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	bad := map[string]map[string]string{
		"empty key":      {"": "v"},
		"long key":       {strings.Repeat("k", MaxMetadataKeyBytes+1): "v"},
		"control in key": {"a\nb": "v"},
		"invalid key":    {"\xff": "v"},
		"long value":     {"k": strings.Repeat("v", MaxMetadataValueBytes+1)},
		"invalid value":  {"k": "\xff"},
		"NUL in value":   {"k": "a\x00b"},
		"too many":       tooMany,
	}
	if len(tooMany) != MaxMetadataEntries+1 {
		t.Fatalf("test setup: %d entries", len(tooMany))
	}
	for name, m := range bad {
		if err := ValidateMetadata(m); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}
