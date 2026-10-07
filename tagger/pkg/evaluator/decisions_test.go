package evaluator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// decisionsStub answers like a decisions vendor, with probabilities taken from probs by tag.
type decisionsStub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []map[string]any
	paths  []string
	probs  map[string]float64
	status int
	// raw replaces the generated answer when set
	raw string
}

// newOpenAIStub plays OpenAI's Decisions API: it answers each predicate by looking the tag up in the
// question's instructions.
func newOpenAIStub(t *testing.T, probs map[string]float64) *decisionsStub {
	t.Helper()
	s := &decisionsStub{probs: probs, status: 200}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.reqs, s.paths = append(s.reqs, body), append(s.paths, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(s.status)
		if s.raw != "" {
			w.Write([]byte(s.raw))
			return
		}
		var answers []map[string]any
		for _, q := range body["questions"].([]any) {
			q := q.(map[string]any)
			for tag, p := range s.probs {
				if strings.Contains(q["instructions"].(string), `"`+tag+`"`) {
					if p < 0 { // a refusal
						answers = append(answers, map[string]any{"type": "refusal", "name": q["name"]})
					} else {
						answers = append(answers, map[string]any{"type": "predicate", "name": q["name"], "probability": p})
					}
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// newVercelStub plays the Vercel evaluate endpoint: answers are keyed by the question names (tags).
func newVercelStub(t *testing.T, probs map[string]float64) *decisionsStub {
	t.Helper()
	s := &decisionsStub{probs: probs, status: 200}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.reqs, s.paths = append(s.reqs, body), append(s.paths, r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(s.status)
		if s.raw != "" {
			w.Write([]byte(s.raw))
			return
		}
		answers := map[string]any{}
		for name := range body["questions"].(map[string]any) {
			if p, ok := s.probs[name]; ok {
				answers[name] = map[string]any{"probability": p}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *decisionsStub) openai(t *testing.T, extra map[string]string) *Decisions {
	t.Helper()
	kv := map[string]string{"TAGGER_DECISIONS_OPENAI_BASE_URL": s.srv.URL, "TAGGER_DECISIONS_OPENAI_API_KEY": "sk-test"}
	for k, v := range extra {
		kv["TAGGER_DECISIONS_OPENAI_"+k] = v
	}
	ev, err := NewDecisionsOpenAI(settings(kv))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func (s *decisionsStub) vercel(t *testing.T, extra map[string]string) *Decisions {
	t.Helper()
	kv := map[string]string{"TAGGER_DECISIONS_VERCEL_BASE_URL": s.srv.URL, "TAGGER_DECISIONS_VERCEL_API_KEY": "vk"}
	for k, v := range extra {
		kv["TAGGER_DECISIONS_VERCEL_"+k] = v
	}
	ev, err := NewDecisionsVercel(settings(kv))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestDecisionsOpenAIRequestShape(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"golang": 0.92, "java": 0.1})
	got, err := s.openai(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("a golang job"), []string{"golang", "java"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["golang"] || got["java"] {
		t.Fatalf("answer = %v", got)
	}
	if len(s.reqs) != 1 || s.paths[0] != "/decisions" {
		t.Fatalf("requests: %v to %v", len(s.reqs), s.paths)
	}
	body := s.reqs[0]
	if body["model"] != "gpt-6-luna" || body["input"] != "a golang job" {
		t.Errorf("body = %v", body)
	}
	qs := body["questions"].([]any)
	if len(qs) != 2 {
		t.Fatalf("questions = %v", qs)
	}
	for i, q := range qs {
		q := q.(map[string]any)
		if q["type"] != "predicate" || q["name"] != fmt.Sprintf("q%d", i) || q["instructions"] == "" {
			t.Errorf("question %d = %v", i, q)
		}
	}
	if !strings.Contains(qs[0].(map[string]any)["instructions"].(string), `"golang"`) {
		t.Errorf("the instructions do not mention the tag: %v", qs[0])
	}
}

func TestDecisionsOpenAIThresholdRefusalAndMissingAnswers(t *testing.T) {
	probs := map[string]float64{"high": 0.9, "edge": 0.5, "low": 0.49, "refused": -1}
	tags := []string{"high", "edge", "low", "refused", "unanswered"}

	s := newOpenAIStub(t, probs)
	got, err := s.openai(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"high": true, "edge": true, "low": false, "refused": false, "unanswered": false}
	for tag, w := range want {
		if got[tag] != w {
			t.Errorf("%s = %v, want %v", tag, got[tag], w)
		}
	}

	got, err = s.openai(t, map[string]string{"THRESHOLD": "0.95"}).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags)
	if err != nil || got["high"] {
		t.Fatalf("with threshold 0.95, high = %v, %v", got["high"], err)
	}
	got, _ = s.openai(t, map[string]string{"THRESHOLD": "0"}).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags)
	if !got["low"] || got["unanswered"] {
		t.Fatalf("with threshold 0, an answered tag is true and an unanswered one is still false: %v", got)
	}
}

func TestDecisionsBatching(t *testing.T) {
	tags := []string{"a", "b", "c", "d", "e"}
	probs := map[string]float64{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1}

	s := newOpenAIStub(t, probs)
	got, err := s.openai(t, map[string]string{"BATCH_SIZE": "2"}).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.reqs) != 3 {
		t.Fatalf("5 tags in batches of 2 took %d requests, want 3", len(s.reqs))
	}
	for _, tag := range tags {
		if !got[tag] {
			t.Errorf("%s lost across batches: %v", tag, got)
		}
	}
	if n := len(s.reqs[2]["questions"].([]any)); n != 1 {
		t.Errorf("last batch has %d questions, want 1", n)
	}

	s = newOpenAIStub(t, probs)
	if _, err := s.openai(t, map[string]string{"BATCH_SIZE": "0"}).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags); err != nil || len(s.reqs) != 1 {
		t.Fatalf("batch size 0 must send everything at once: %d requests, %v", len(s.reqs), err)
	}
	s = newOpenAIStub(t, probs)
	if _, err := s.openai(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags); err != nil || len(s.reqs) != 1 {
		t.Fatalf("the default batch holds 50: %d requests, %v", len(s.reqs), err)
	}

	// A failing batch fails the evaluation: a partial answer is not a verdict.
	s = newOpenAIStub(t, probs)
	s.status = http.StatusBadGateway
	if _, err := s.openai(t, map[string]string{"BATCH_SIZE": "2"}).Evaluate(context.Background(), DataTypeTxt, []byte("x"), tags); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDecisionsCustomInstructionsAndVendorSettings(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"golang": 1})
	ev := s.openai(t, map[string]string{
		"INSTRUCTIONS": `Does this text concern "{tag}"? Be strict.`, "PATH": "/v2/decide", "MODEL": "luna-2",
		"PARAMS": `{"store":false}`,
	})
	if _, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"golang"}); err != nil {
		t.Fatal(err)
	}
	q := s.reqs[0]["questions"].([]any)[0].(map[string]any)
	if q["instructions"] != `Does this text concern "golang"? Be strict.` || s.paths[0] != "/v2/decide" ||
		s.reqs[0]["model"] != "luna-2" || s.reqs[0]["store"] != false {
		t.Errorf("request = %v to %s", s.reqs[0], s.paths[0])
	}
}

func TestDecisionsRejectBadSettings(t *testing.T) {
	for name, kv := range map[string]map[string]string{
		"threshold above 1":     {"THRESHOLD": "1.5"},
		"threshold negative":    {"THRESHOLD": "-0.1"},
		"threshold not numeric": {"THRESHOLD": "high"},
		"negative batch size":   {"BATCH_SIZE": "-1"},
		"batch size not a int":  {"BATCH_SIZE": "many"},
		"instructions no tag":   {"INSTRUCTIONS": "Is it good?"},
		"bad base url":          {"BASE_URL": "nope"},
	} {
		t.Run(name, func(t *testing.T) {
			full := map[string]string{}
			for k, v := range kv {
				full["TAGGER_DECISIONS_OPENAI_"+k] = v
			}
			if _, err := NewDecisionsOpenAI(settings(full)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestDecisionsEdgeCases(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"a": 1})
	ev := s.openai(t, nil)

	got, err := ev.Evaluate(context.Background(), DataType("png"), []byte("x"), []string{"a"})
	if err != nil || got["a"] || len(s.reqs) != 0 {
		t.Fatalf("non-txt: %v, %v, requests %d", got, err, len(s.reqs))
	}
	got, err = ev.Evaluate(context.Background(), DataTypeTxt, []byte("x"), nil)
	if err != nil || len(got) != 0 || len(s.reqs) != 0 {
		t.Fatalf("no tags: %v, %v, requests %d", got, err, len(s.reqs))
	}
	if types := ev.GetSupportedDataTypes(); len(types) != 1 || types[0] != "txt" {
		t.Fatalf("types = %v", types)
	}

	for name, raw := range map[string]string{"not json": `nope`, "answers wrong type": `{"answers":"x"}`} {
		s.raw = raw
		if _, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"a"}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	s.raw = `{"answers":[{"type":"predicate","name":"q0"}]}` // no probability: unanswered, not zero
	got, err = ev.Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"a"})
	if err != nil || got["a"] {
		t.Fatalf("an answer without a probability is no answer: %v, %v", got, err)
	}
}

func TestDecisionsVercelRequestShapeAndAnswers(t *testing.T) {
	s := newVercelStub(t, map[string]float64{"golang": 0.92, "java": 0.2, "rust": 0.5})
	got, err := s.vercel(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("a golang job"), []string{"golang", "java", "rust", "go lang & more"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["golang"] || got["java"] || !got["rust"] || got["go lang & more"] {
		t.Fatalf("answer = %v", got)
	}
	if s.paths[0] != "/evaluate" {
		t.Errorf("path = %s", s.paths[0])
	}
	body := s.reqs[0]
	if body["model"] != "typesafe-ai/jev" || body["state"] != "a golang job" {
		t.Errorf("body = %v", body)
	}
	qs := body["questions"].(map[string]any)
	q, ok := qs["go lang & more"].(map[string]any) // questions are keyed by the tag, whatever it contains
	if !ok || q["type"] != "boolean" || !strings.Contains(q["instructions"].(string), "go lang & more") {
		t.Errorf("questions = %v", qs)
	}
}

func TestDecisionsVercelSettingsAndErrors(t *testing.T) {
	s := newVercelStub(t, map[string]float64{"a": 0.6})
	got, err := s.vercel(t, map[string]string{"THRESHOLD": "0.7", "MODEL": "m2", "PATH": "/v2/evaluate"}).
		Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"a"})
	if err != nil || got["a"] || s.reqs[0]["model"] != "m2" || s.paths[0] != "/v2/evaluate" {
		t.Fatalf("got %v, %v, %v to %s", got, err, s.reqs[0]["model"], s.paths[0])
	}
	s.status = http.StatusUnauthorized
	if _, err := s.vercel(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"a"}); err == nil {
		t.Fatal("expected an error")
	}
	s.status, s.raw = 200, `{"answers":`
	if _, err := s.vercel(t, nil).Evaluate(context.Background(), DataTypeTxt, []byte("x"), []string{"a"}); err == nil {
		t.Fatal("expected an error for a malformed answer")
	}
}
