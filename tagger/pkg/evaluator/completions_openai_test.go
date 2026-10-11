package evaluator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type completionsStub struct {
	srv  *httptest.Server
	req  *http.Request
	body map[string]any
	// content is the assistant message to answer with
	content string
	status  int
}

func newCompletionsStub(t *testing.T) *completionsStub {
	t.Helper()
	s := &completionsStub{status: 200, content: `{"golang": true, "java": false}`}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.req = r
		s.body = map[string]any{}
		json.NewDecoder(r.Body).Decode(&s.body)
		w.WriteHeader(s.status)
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": s.content}}}})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *completionsStub) evaluator(t *testing.T, extra map[string]string) *CompletionsOpenAI {
	t.Helper()
	kv := map[string]string{"TAGGER_COMPLETIONS_OPENAI_BASE_URL": s.srv.URL, "TAGGER_COMPLETIONS_OPENAI_API_KEY": "sk-test"}
	for k, v := range extra {
		kv["TAGGER_COMPLETIONS_OPENAI_"+k] = v
	}
	ev, err := NewCompletionsOpenAI(settings(kv))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestCompletionsOpenAIRequestAndAnswer(t *testing.T) {
	s := newCompletionsStub(t)
	got, err := s.evaluator(t, nil).Evaluate(context.Background(), Text("a golang job"), []string{"golang", "java", "rust"})
	if err != nil {
		t.Fatal(err)
	}
	if !got["golang"] || got["java"] || got["rust"] {
		t.Fatalf("answer = %v (a tag the model leaves out is false)", got)
	}
	if s.req.URL.Path != "/chat/completions" || s.req.Header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("request = %s, auth %q", s.req.URL.Path, s.req.Header.Get("Authorization"))
	}
	if s.body["model"] != "gpt-4o-mini" || s.body["temperature"] != float64(0) {
		t.Errorf("body = %v", s.body)
	}
	msgs := s.body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != DefaultCompletionsSystemPrompt {
		t.Errorf("messages = %v", msgs)
	}
}

func TestCompletionsOpenAIConfigurableForOtherVendors(t *testing.T) {
	s := newCompletionsStub(t)
	ev := s.evaluator(t, map[string]string{
		"PATH": "/openai/deployments/d1/chat/completions", "MODEL": "d1", "AUTH_HEADER": "api-key", "AUTH_SCHEME": "",
		"QUERY": "api-version=2024-10-21", "HEADERS": `{"X-Tenant":"t1"}`,
		"PARAMS": `{"temperature":null,"max_completion_tokens":64,"reasoning_effort":"low"}`, "SYSTEM_PROMPT": "Answer in JSON.",
	})
	if _, err := ev.Evaluate(context.Background(), Text("x"), []string{"golang"}); err != nil {
		t.Fatal(err)
	}
	if s.req.URL.Path != "/openai/deployments/d1/chat/completions" || s.req.URL.Query().Get("api-version") != "2024-10-21" {
		t.Errorf("url = %s", s.req.URL)
	}
	if s.req.Header.Get("Api-Key") != "sk-test" || s.req.Header.Get("Authorization") != "" || s.req.Header.Get("X-Tenant") != "t1" {
		t.Errorf("headers = %v", s.req.Header)
	}
	if _, has := s.body["temperature"]; has || s.body["max_completion_tokens"] != float64(64) || s.body["reasoning_effort"] != "low" || s.body["model"] != "d1" {
		t.Errorf("body = %v", s.body)
	}
	if msgs := s.body["messages"].([]any); msgs[0].(map[string]any)["content"] != "Answer in JSON." {
		t.Errorf("system prompt = %v", msgs[0])
	}
}

func TestCompletionsOpenAIParsesMessyAnswers(t *testing.T) {
	tests := map[string]struct {
		content string
		want    bool
		wantErr bool
	}{
		"plain":          {`{"golang": true}`, true, false},
		"code fence":     {"```json\n{\"golang\": true}\n```", true, false},
		"reasoning":      {"<think>{\"golang\": false}</think>{\"golang\": true}", true, false},
		"string value":   {`{"golang": "true"}`, true, false},
		"number value":   {`{"golang": 1}`, true, false},
		"not json":       {`I think so`, false, true},
		"unrelated tags": {`{"python": true}`, false, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := newCompletionsStub(t)
			s.content = tt.content
			got, err := s.evaluator(t, nil).Evaluate(context.Background(), Text("x"), []string{"golang"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && got["golang"] != tt.want {
				t.Fatalf("golang = %v, want %v", got["golang"], tt.want)
			}
		})
	}
}

func TestCompletionsOpenAIEdgeCases(t *testing.T) {
	s := newCompletionsStub(t)
	ev := s.evaluator(t, nil)

	got, err := ev.Evaluate(context.Background(), Text("x"), nil)
	if err != nil || len(got) != 0 || s.req != nil {
		t.Fatalf("no tags: %v, %v, vendor called: %v", got, err, s.req != nil)
	}
	s.status = http.StatusInternalServerError
	if _, err := ev.Evaluate(context.Background(), Text("x"), []string{"a"}); err == nil {
		t.Fatal("a vendor error must be an error")
	}
	if v := ev.Version(); v != "completions/openai:"+ev.cfg.Model || ev.cfg.Model == "" {
		t.Fatalf("version = %q", v)
	}
}

// An image goes to the chat completions API as an image_url part with a base64 data URL of the stored bytes.
func TestCompletionsOpenAISendsAnImageAsIs(t *testing.T) {
	s := newCompletionsStub(t)
	bytes := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe}
	got, err := s.evaluator(t, nil).Evaluate(context.Background(), Content{Data: bytes, Type: "image/png"}, []string{"golang", "java"})
	if err != nil || !got["golang"] || got["java"] {
		t.Fatalf("got %v, %v", got, err)
	}
	msgs := s.body["messages"].([]any)
	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("user message = %v", user)
	}
	parts := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("content = %v", parts)
	}
	text := parts[0].(map[string]any)
	if text["type"] != "text" || !strings.Contains(text["text"].(string), `["golang", "java"]`) || !strings.Contains(text["text"].(string), "image") {
		t.Errorf("text part = %v", text)
	}
	img := parts[1].(map[string]any)
	url := img["image_url"].(map[string]any)["url"]
	if img["type"] != "image_url" || url != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(bytes) {
		t.Errorf("image part = %v", img)
	}
	if _, has := img["image_url"].(map[string]any)["detail"]; has {
		t.Errorf("detail is left to the API: %v", img)
	}
}

func TestCompletionsOpenAINonImagesStayText(t *testing.T) {
	s := newCompletionsStub(t)
	for _, typ := range []string{"", "text/plain", "application/pdf"} {
		if _, err := s.evaluator(t, nil).Evaluate(context.Background(), Content{Data: []byte("hello"), Type: typ}, []string{"golang"}); err != nil {
			t.Fatal(err)
		}
		content, ok := s.body["messages"].([]any)[1].(map[string]any)["content"].(string)
		if !ok || !strings.Contains(content, "hello") {
			t.Errorf("%q: content = %v, want the text prompt", typ, s.body["messages"].([]any)[1])
		}
	}
}

func TestCompletionsOpenAIImageIsNotFilteredByFormat(t *testing.T) {
	s := newCompletionsStub(t)
	if _, err := s.evaluator(t, nil).Evaluate(context.Background(), Content{Data: []byte("x"), Type: "image/tiff"}, []string{"golang"}); err != nil {
		t.Fatal(err)
	}
	parts := s.body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(url, "data:image/tiff;base64,") {
		t.Errorf("url = %s", url)
	}
}
