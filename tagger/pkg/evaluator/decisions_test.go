package evaluator

import (
	"context"
	"encoding/base64"
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
	got, err := s.openai(t, nil).Evaluate(context.Background(), Text("a golang job"), []string{"golang", "java"})
	if err != nil {
		t.Fatal(err)
	}
	if got["golang"] != 0.92 || got["java"] != 0.1 {
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

func TestDecisionsOpenAIReportsProbabilityRefusalAndMissingAnswers(t *testing.T) {
	probs := map[string]float64{"high": 0.9, "edge": 0.5, "low": 0.49, "refused": -1}
	tags := []string{"high", "edge", "low", "refused", "unanswered"}

	s := newOpenAIStub(t, probs)
	got, err := s.openai(t, nil).Evaluate(context.Background(), Text("x"), tags)
	if err != nil {
		t.Fatal(err)
	}
	// the vendor's probability is the confidence as it is; a refusal or no answer is 0
	want := map[string]float64{"high": 0.9, "edge": 0.5, "low": 0.49, "refused": 0, "unanswered": 0}
	for tag, w := range want {
		if got[tag] != w {
			t.Errorf("%s = %v, want %v", tag, got[tag], w)
		}
	}
}

func TestDecisionsBatching(t *testing.T) {
	tags := []string{"a", "b", "c", "d", "e"}
	probs := map[string]float64{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1}

	s := newOpenAIStub(t, probs)
	got, err := s.openai(t, map[string]string{"BATCH_SIZE": "2"}).Evaluate(context.Background(), Text("x"), tags)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.reqs) != 3 {
		t.Fatalf("5 tags in batches of 2 took %d requests, want 3", len(s.reqs))
	}
	for _, tag := range tags {
		if got[tag] != 1 {
			t.Errorf("%s lost across batches: %v", tag, got)
		}
	}
	if n := len(s.reqs[2]["questions"].([]any)); n != 1 {
		t.Errorf("last batch has %d questions, want 1", n)
	}

	s = newOpenAIStub(t, probs)
	if _, err := s.openai(t, map[string]string{"BATCH_SIZE": "0"}).Evaluate(context.Background(), Text("x"), tags); err != nil || len(s.reqs) != 1 {
		t.Fatalf("batch size 0 must send everything at once: %d requests, %v", len(s.reqs), err)
	}
	s = newOpenAIStub(t, probs)
	if _, err := s.openai(t, nil).Evaluate(context.Background(), Text("x"), tags); err != nil || len(s.reqs) != 1 {
		t.Fatalf("the default batch holds 50: %d requests, %v", len(s.reqs), err)
	}

	// A failing batch fails the evaluation: a partial answer is not a verdict.
	s = newOpenAIStub(t, probs)
	s.status = http.StatusBadGateway
	if _, err := s.openai(t, map[string]string{"BATCH_SIZE": "2"}).Evaluate(context.Background(), Text("x"), tags); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDecisionsCustomInstructionsAndVendorSettings(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"golang": 1})
	ev := s.openai(t, map[string]string{
		"INSTRUCTIONS": `Does this text concern "{tag}"? Be strict.`, "PATH": "/v2/decide", "MODEL": "luna-2",
		"PARAMS": `{"store":false}`,
	})
	if _, err := ev.Evaluate(context.Background(), Text("x"), []string{"golang"}); err != nil {
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
		"negative batch size":  {"BATCH_SIZE": "-1"},
		"batch size not a int": {"BATCH_SIZE": "many"},
		"instructions no tag":  {"INSTRUCTIONS": "Is it good?"},
		"bad base url":         {"BASE_URL": "nope"},
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

	got, err := ev.Evaluate(context.Background(), Text("x"), nil)
	if err != nil || len(got) != 0 || len(s.reqs) != 0 {
		t.Fatalf("no tags: %v, %v, requests %d", got, err, len(s.reqs))
	}
	if v := ev.Version(); v != "decisions/openai:"+ev.cfg.Model || ev.cfg.Model == "" {
		t.Fatalf("version = %q", v)
	}

	for name, raw := range map[string]string{"not json": `nope`, "answers wrong type": `{"answers":"x"}`} {
		s.raw = raw
		if _, err := ev.Evaluate(context.Background(), Text("x"), []string{"a"}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	s.raw = `{"answers":[{"type":"predicate","name":"q0"}]}` // no probability: unanswered, not zero
	got, err = ev.Evaluate(context.Background(), Text("x"), []string{"a"})
	if err != nil || got["a"] != 0 {
		t.Fatalf("an answer without a probability is no answer: %v, %v", got, err)
	}
}

func TestDecisionsVercelRequestShapeAndAnswers(t *testing.T) {
	s := newVercelStub(t, map[string]float64{"golang": 0.92, "java": 0.2, "rust": 0.5})
	got, err := s.vercel(t, nil).Evaluate(context.Background(), Text("a golang job"), []string{"golang", "java", "rust", "go lang & more"})
	if err != nil {
		t.Fatal(err)
	}
	if got["golang"] != 0.92 || got["java"] != 0.2 || got["rust"] != 0.5 || got["go lang & more"] != 0 {
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
	got, err := s.vercel(t, map[string]string{"MODEL": "m2", "PATH": "/v2/evaluate"}).
		Evaluate(context.Background(), Text("x"), []string{"a"})
	if err != nil || got["a"] != 0.6 || s.reqs[0]["model"] != "m2" || s.paths[0] != "/v2/evaluate" {
		t.Fatalf("got %v, %v, %v to %s", got, err, s.reqs[0]["model"], s.paths[0])
	}
	s.status = http.StatusUnauthorized
	if _, err := s.vercel(t, nil).Evaluate(context.Background(), Text("x"), []string{"a"}); err == nil {
		t.Fatal("expected an error")
	}
	s.status, s.raw = 200, `{"answers":`
	if _, err := s.vercel(t, nil).Evaluate(context.Background(), Text("x"), []string{"a"}); err == nil {
		t.Fatal("expected an error for a malformed answer")
	}
}

func TestContentIsImage(t *testing.T) {
	for typ, want := range map[string]bool{
		"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true,
		"IMAGE/PNG": true, "image/png; charset=binary": true,
		"image/tiff": true, // any image type: which formats the vendor takes is up to the uploader
		"text/plain": false, "application/pdf": false, "application/octet-stream": false,
		"": false, "image": false, "not a type": false,
	} {
		if got := (Content{Type: typ}).IsImage(); got != want {
			t.Errorf("IsImage(%q) = %v, want %v", typ, got, want)
		}
	}
	if got := (Content{Type: "IMAGE/PNG; a=b"}).MediaType(); got != "image/png" {
		t.Errorf("MediaType = %q", got)
	}
}

// An image goes to the Decisions API as an input_image with a base64 data URL of the stored bytes, untouched.
func TestDecisionsOpenAISendsAnImageAsIs(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"cat": 0.9, "dog": 0.1})
	bytes := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe, 0x80} // not valid UTF-8
	got, err := s.openai(t, nil).Evaluate(context.Background(), Content{Data: bytes, Type: "image/png"}, []string{"cat", "dog"})
	if err != nil {
		t.Fatal(err)
	}
	if got["cat"] != 0.9 || got["dog"] != 0.1 {
		t.Fatalf("got %v", got)
	}
	req := s.reqs[0]
	input := req["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v", msg["role"])
	}
	parts := msg["content"].([]any)
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "input_text" {
		t.Fatalf("content = %v", parts)
	}
	img := parts[1].(map[string]any)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bytes)
	if img["type"] != "input_image" || img["image_url"] != want {
		t.Errorf("image part = %v, want url %s", img, want)
	}
	if _, has := img["detail"]; has {
		t.Errorf("detail is left to the API: %v", img)
	}
	// the questions are about the image
	for _, q := range req["questions"].([]any) {
		ins := q.(map[string]any)["instructions"].(string)
		if !strings.Contains(ins, "Analyze the image") {
			t.Errorf("instructions = %q", ins)
		}
	}
}

func TestDecisionsOpenAIImageIsNotFilteredByFormat(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"a": 1})
	for _, typ := range []string{"image/tiff", "image/svg+xml", "IMAGE/JPEG; q=1"} {
		s.reqs = nil
		if _, err := s.openai(t, nil).Evaluate(context.Background(), Content{Data: []byte("x"), Type: typ}, []string{"a"}); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		parts := s.reqs[0]["input"].([]any)[0].(map[string]any)["content"].([]any)
		url := parts[1].(map[string]any)["image_url"].(string)
		wantType := strings.ToLower(strings.SplitN(typ, ";", 2)[0])
		if !strings.HasPrefix(url, "data:"+wantType+";base64,") {
			t.Errorf("%s: url = %s", typ, url)
		}
	}
}

func TestDecisionsOpenAINonImagesStayText(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"a": 1})
	for _, typ := range []string{"", "text/plain", "application/pdf", "application/octet-stream"} {
		s.reqs = nil
		if _, err := s.openai(t, nil).Evaluate(context.Background(), Content{Data: []byte("hello"), Type: typ}, []string{"a"}); err != nil {
			t.Fatal(err)
		}
		if s.reqs[0]["input"] != "hello" {
			t.Errorf("%q: input = %v, want the text", typ, s.reqs[0]["input"])
		}
		ins := s.reqs[0]["questions"].([]any)[0].(map[string]any)["instructions"].(string)
		if !strings.Contains(ins, "Analyze the text") {
			t.Errorf("%q: instructions = %q", typ, ins)
		}
	}
}

func TestDecisionsOpenAIImageBatches(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"a": 1, "b": 1, "c": 0})
	got, err := s.openai(t, map[string]string{"BATCH_SIZE": "2"}).Evaluate(context.Background(), Content{Data: []byte{1, 2, 3}, Type: "image/webp"}, []string{"a", "b", "c"})
	if err != nil || got["a"] != 1 || got["b"] != 1 || got["c"] != 0 || len(s.reqs) != 2 {
		t.Fatalf("got %v, %d requests, %v", got, len(s.reqs), err)
	}
	for _, r := range s.reqs {
		if _, ok := r["input"].([]any); !ok {
			t.Errorf("a batch is not an image request: %v", r["input"])
		}
	}
}

func TestDecisionsImageInstructionsSetting(t *testing.T) {
	s := newOpenAIStub(t, map[string]float64{"a": 1})
	ev := s.openai(t, map[string]string{"IMAGE_INSTRUCTIONS": `Does the picture show "{tag}"?`})
	if _, err := ev.Evaluate(context.Background(), Content{Data: []byte{1}, Type: "image/png"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if ins := s.reqs[0]["questions"].([]any)[0].(map[string]any)["instructions"]; ins != `Does the picture show "a"?` {
		t.Errorf("instructions = %v", ins)
	}
	// text still uses INSTRUCTIONS
	if _, err := ev.Evaluate(context.Background(), Content{Data: []byte("t")}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if ins := s.reqs[1]["questions"].([]any)[0].(map[string]any)["instructions"].(string); !strings.Contains(ins, "Analyze the text") {
		t.Errorf("text instructions = %q", ins)
	}
	if _, err := NewDecisionsOpenAI(settings(map[string]string{"TAGGER_DECISIONS_OPENAI_API_KEY": "k", "TAGGER_DECISIONS_OPENAI_IMAGE_INSTRUCTIONS": "no placeholder"})); err == nil {
		t.Error("IMAGE_INSTRUCTIONS without {tag}: no error")
	}
}

// The Vercel dialect has no image support here: an image is read as text, as before.
func TestDecisionsVercelReadsAnImageAsText(t *testing.T) {
	s := newVercelStub(t, map[string]float64{"a": 1})
	if _, err := s.vercel(t, nil).Evaluate(context.Background(), Content{Data: []byte("pixels"), Type: "image/png"}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if s.reqs[0]["state"] != "pixels" {
		t.Errorf("state = %v", s.reqs[0]["state"])
	}
}
