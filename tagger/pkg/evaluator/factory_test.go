package evaluator

import (
	"strings"
	"testing"
)

func TestNewSelectsEvaluators(t *testing.T) {
	tests := []struct {
		impl string
		want any
	}{
		{"grep", &GrepEvaluator{}},
		{"false", &FalseEvaluator{}},
		{"completions/openai", &CompletionsOpenAI{}},
		{"decisions/openai", &Decisions{}},
		{"decisions/vercel", &Decisions{}},
	}
	for _, tt := range tests {
		t.Run(tt.impl, func(t *testing.T) {
			ev, err := New(tt.impl, settings(nil))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := typeName(ev), typeName(tt.want); got != want {
				t.Fatalf("New(%q) = %s, want %s", tt.impl, got, want)
			}
			if d, ok := ev.(*Decisions); ok && d.label != tt.impl {
				t.Errorf("label = %q", d.label)
			}
		})
	}
}

func typeName(v any) string {
	switch v.(type) {
	case *GrepEvaluator:
		return "grep"
	case *FalseEvaluator:
		return "false"
	case *CompletionsOpenAI:
		return "completions"
	case *Decisions:
		return "decisions"
	}
	return "?"
}

func TestNewNamesTheReplacementOfRenamedEvaluators(t *testing.T) {
	for old, replacement := range map[string]string{"openai": "completions/openai", "systemone": "decisions/vercel"} {
		_, err := New(old, settings(nil))
		if err == nil || !strings.Contains(err.Error(), replacement) || !strings.Contains(err.Error(), "renamed") {
			t.Errorf("New(%q): %v", old, err)
		}
	}
}

func TestNewListsTheAvailableEvaluators(t *testing.T) {
	_, err := New("gpt", settings(nil))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range Names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the error does not list %q: %v", name, err)
		}
	}
}

func TestNewReportsBadSettingsOfTheSelectedEvaluator(t *testing.T) {
	for impl, key := range map[string]string{
		"completions/openai": "TAGGER_COMPLETIONS_OPENAI_PARAMS",
		"decisions/openai":   "TAGGER_DECISIONS_OPENAI_THRESHOLD",
		"decisions/vercel":   "TAGGER_DECISIONS_VERCEL_BATCH_SIZE",
	} {
		ev, err := New(impl, settings(map[string]string{key: "nonsense"}))
		if err == nil || ev != nil || !strings.Contains(err.Error(), key) {
			t.Errorf("New(%q) with a bad %s = %v, %v", impl, key, ev, err)
		}
	}
	// Settings of one evaluator do not affect another.
	if _, err := New("decisions/openai", settings(map[string]string{"TAGGER_DECISIONS_VERCEL_THRESHOLD": "nonsense"})); err != nil {
		t.Errorf("a vercel setting broke decisions/openai: %v", err)
	}
}

func TestVersions(t *testing.T) {
	lookup := func(env map[string]string) LookupFunc {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}
	tests := []struct {
		impl string
		env  map[string]string
		want string
	}{
		{"grep", nil, "grep"},
		{"false", nil, "false"},
		{"completions/openai", map[string]string{"TAGGER_COMPLETIONS_OPENAI_API_KEY": "k", "TAGGER_COMPLETIONS_OPENAI_MODEL": "zai-org/GLM-5.3-Flash"}, "completions/openai:zai-org/GLM-5.3-Flash"},
		{"decisions/openai", map[string]string{"TAGGER_DECISIONS_OPENAI_API_KEY": "k", "TAGGER_DECISIONS_OPENAI_MODEL": "m1"}, "decisions/openai:m1"},
		{"decisions/vercel", map[string]string{"TAGGER_DECISIONS_VERCEL_API_KEY": "k"}, "decisions/vercel:typesafe-ai/jev"},
	}
	for _, tt := range tests {
		t.Run(tt.impl, func(t *testing.T) {
			ev, err := New(tt.impl, lookup(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if got := ev.Version(); got != tt.want {
				t.Fatalf("Version() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateVersion(t *testing.T) {
	for _, ok := range []string{"grep", "decisions/openai:zai-org/GLM-5.3-Flash", strings.Repeat("x", MaxVersionBytes), "naïve:模型"} {
		if err := ValidateVersion(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", MaxVersionBytes+1), "a\nb", "a\x00b", "\xff"} {
		if err := ValidateVersion(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
