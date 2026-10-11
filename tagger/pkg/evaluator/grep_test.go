package evaluator

import (
	"context"
	"testing"
)

// Every requested tag is answered, with 1 or 0: a tag left out of the answer would not be stored and would be
// evaluated again by every query.
func TestGrepAnswersEveryTag(t *testing.T) {
	got, err := NewGrepEvaluator().Evaluate(context.Background(), Text("a golang job"), []string{"golang", "java"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["golang"] != 1 {
		t.Fatalf("got %v", got)
	}
	if v, ok := got["java"]; !ok || v != 0 {
		t.Fatalf("java = %v, present %v; want 0, present", v, ok)
	}
}

func TestFalseAnswersEveryTag(t *testing.T) {
	got, _ := NewFalseEvaluator().Evaluate(context.Background(), Text("x"), []string{"a", "b"})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
