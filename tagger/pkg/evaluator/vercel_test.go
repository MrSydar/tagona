package evaluator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestVercelEvaluator(t *testing.T, threshold float64, handler http.HandlerFunc) *VercelEvaluator {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewVercelEvaluator("test-key", srv.URL, "typesafe-ai/jev", threshold, time.Second)
}

func TestVercelEvaluatorRequestShape(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotAuth   string
		gotType   string
		gotBody   evaluateRequest
	)
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("unmarshal request body: %v", err)
		}
		w.Write([]byte(`{"answers":{"is_urgent":{"probability":0.87}}}`))
	})

	result, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("I was charged twice for my subscription."), []string{"is_urgent"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/evaluate" {
		t.Errorf("path = %q, want /evaluate", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want %q", gotAuth, "Bearer test-key")
	}
	if gotType != "application/json" {
		t.Errorf("content type = %q, want application/json", gotType)
	}
	if gotBody.Model != "typesafe-ai/jev" {
		t.Errorf("model = %q, want typesafe-ai/jev", gotBody.Model)
	}
	if gotBody.State != "I was charged twice for my subscription." {
		t.Errorf("state = %q, want the object content", gotBody.State)
	}
	q, ok := gotBody.Questions["is_urgent"]
	if !ok {
		t.Fatalf("questions missing key is_urgent: %+v", gotBody.Questions)
	}
	if q.Type != "boolean" {
		t.Errorf("question type = %q, want boolean", q.Type)
	}
	if q.Instructions == "" {
		t.Errorf("question instructions empty, want derived instructions for tag")
	}

	want := map[string]bool{"is_urgent": true}
	for tag, v := range want {
		if result[tag] != v {
			t.Errorf("result[%q] = %v, want %v", tag, result[tag], v)
		}
	}
}

func TestVercelEvaluatorThreshold(t *testing.T) {
	probability := func(p float64) http.HandlerFunc {
		resp, _ := json.Marshal(evaluateResponse{Answers: map[string]evaluateAnswer{"tag": {Probability: p}}})
		return func(w http.ResponseWriter, r *http.Request) {
			w.Write(resp)
		}
	}
	cases := []struct {
		name        string
		threshold   float64
		probability float64
		want        bool
	}{
		{"above default threshold", 0.5, 0.87, true},
		{"below default threshold", 0.5, 0.49, false},
		{"exactly at default threshold", 0.5, 0.5, true},
		{"below custom threshold", 0.9, 0.87, false},
		{"exactly at custom threshold", 0.9, 0.9, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := newTestVercelEvaluator(t, tc.threshold, probability(tc.probability))
			result, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("some text"), []string{"tag"})
			if err != nil {
				t.Fatalf("Evaluate returned error: %v", err)
			}
			if result["tag"] != tc.want {
				t.Errorf("result[tag] = %v (probability %v, threshold %v), want %v", result["tag"], tc.probability, tc.threshold, tc.want)
			}
		})
	}
}

func TestVercelEvaluatorNonTxtDataType(t *testing.T) {
	calls := 0
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"answers":{}}`))
	})

	result, err := ev.Evaluate(context.Background(), DataTypePng, []byte{0x89, 0x50}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if calls != 0 {
		t.Errorf("handler called %d times, want 0", calls)
	}
	want := map[string]bool{"a": false, "b": false}
	for tag, v := range want {
		if result[tag] != v {
			t.Errorf("result[%q] = %v, want %v", tag, result[tag], v)
		}
	}
	if len(result) != 2 {
		t.Errorf("result length = %d, want 2", len(result))
	}
}

func TestVercelEvaluatorEmptyTags(t *testing.T) {
	calls := 0
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"answers":{}}`))
	})

	result, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("some text"), nil)
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if calls != 0 {
		t.Errorf("handler called %d times, want 0", calls)
	}
	if len(result) != 0 {
		t.Errorf("result length = %d, want 0", len(result))
	}
}

func TestVercelEvaluatorMissingAnswerDefaultsFalse(t *testing.T) {
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"present":{"probability":0.9}}}`))
	})

	result, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("some text"), []string{"present", "absent"})
	if err != nil {
		t.Fatalf("Evaluate returned error: %v", err)
	}
	if result["present"] != true {
		t.Errorf("result[present] = %v, want true", result["present"])
	}
	if result["absent"] != false {
		t.Errorf("result[absent] = %v, want false", result["absent"])
	}
}

func TestVercelEvaluatorHTTPError(t *testing.T) {
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	_, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("some text"), []string{"tag"})
	if err == nil {
		t.Fatal("Evaluate returned nil error, want error for non-200 status")
	}
}

func TestVercelEvaluatorMalformedResponse(t *testing.T) {
	ev := newTestVercelEvaluator(t, 0.5, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	})

	_, err := ev.Evaluate(context.Background(), DataTypeTxt, []byte("some text"), []string{"tag"})
	if err == nil {
		t.Fatal("Evaluate returned nil error, want error for malformed response")
	}
}

func TestVercelEvaluatorGetSupportedDataTypes(t *testing.T) {
	ev := NewVercelEvaluator("", "https://ai-gateway.vercel.sh/v1", "typesafe-ai/jev", 0.5, time.Second)
	got := ev.GetSupportedDataTypes()
	if len(got) != 1 || got[0] != string(DataTypeTxt) {
		t.Errorf("GetSupportedDataTypes = %v, want [txt]", got)
	}
}
