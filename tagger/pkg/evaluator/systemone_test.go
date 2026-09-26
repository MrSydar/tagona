package evaluator

import (
	"strings"
	"testing"
)

func TestNewSystemoneEvaluatorVercelBackend(t *testing.T) {
	ev, err := NewSystemoneEvaluator("vercel")
	if err != nil {
		t.Fatalf("NewSystemoneEvaluator(vercel) returned error: %v", err)
	}
	if ev == nil {
		t.Fatal("NewSystemoneEvaluator(vercel) returned nil evaluator")
	}
	if ev.backend != "vercel" {
		t.Errorf("backend = %q, want vercel", ev.backend)
	}
	if ev.inner == nil {
		t.Error("inner evaluator is nil, want vercel backend evaluator")
	}
}

func TestNewSystemoneEvaluatorUnknownBackend(t *testing.T) {
	_, err := NewSystemoneEvaluator("nope")
	if err == nil {
		t.Fatal("NewSystemoneEvaluator(nope) returned nil error, want error")
	}
	if !strings.Contains(err.Error(), "unknown systemone backend") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "unknown systemone backend")
	}
}
