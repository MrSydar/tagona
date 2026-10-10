package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	storageclient "mrsydar/tagona/storage/pkg/client"
	"mrsydar/tagona/tagger/internal/metrics"
	"mrsydar/tagona/tagger/pkg/evaluator"
)

// Server is the tagging engine HTTP server.
type Server struct {
	storage       *storageclient.Client
	evaluator     evaluator.Evaluator
	evaluatorImpl string
	version       string
}

// NewServer creates a new tagging engine server. version is what the engine reports and what a tag
// request must ask for; it is the evaluator's own version unless the operator set another one.
func NewServer(storageClient *storageclient.Client, evaluator evaluator.Evaluator, evaluatorImpl, version string) *Server {
	return &Server{
		storage:       storageClient,
		evaluator:     evaluator,
		evaluatorImpl: evaluatorImpl,
		version:       version,
	}
}

// Router builds the chi router.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Use(metrics.Middleware)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/version", s.versionInfo)
	r.Post("/tag", s.tag)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// versionInfo reports the versions the engine serves: its own, "<implementation>[:<model>]" unless
// TAGGER_VERSION says otherwise. It is a list because a tagger router serves the versions of several taggers.
func (s *Server) versionInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{"version": {s.version}})
}

type tagRequest struct {
	Collection string `json:"collection"`
	ObjectID   string `json:"object_id"`
	// TaggerVersion is the version the object's collection is tagged with. If it is not the version this
	// engine runs, the request is refused with 409: another tagger may tag the object differently, so its
	// answer must not be stored as this collection's.
	TaggerVersion string   `json:"tagger_version"`
	Tags          []string `json:"tags"`
}

type tagResponse struct {
	Tags map[string]bool `json:"tags"`
}

func (s *Server) tag(w http.ResponseWriter, r *http.Request) {
	var req tagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"code":"invalid_request","message":"invalid json"}}`, http.StatusBadRequest)
		return
	}

	if req.Collection == "" || req.ObjectID == "" || req.TaggerVersion == "" {
		http.Error(w, `{"error":{"code":"invalid_request","message":"collection, object_id and tagger_version required"}}`, http.StatusBadRequest)
		return
	}
	for _, tag := range req.Tags {
		if err := validateTag(tag); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"code":"invalid_tag","message":"%s"}}`, err.Error()), http.StatusBadRequest)
			return
		}
	}

	if req.TaggerVersion != s.version {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"code":    "tagger_version_mismatch",
			"message": fmt.Sprintf("this engine serves %q, not %q", s.version, req.TaggerVersion),
			"details": map[string]any{"expected": req.TaggerVersion, "running": []string{s.version}},
		}})
		return
	}

	// Fetch object data.
	data, err := s.storage.GetObjectContent(r.Context(), req.Collection, req.ObjectID)
	if err != nil {
		slog.Error("tagger: fetch data failed", "error", err)
		http.Error(w, `{"error":{"code":"storage_error","message":"failed to fetch data"}}`, http.StatusBadGateway)
		return
	}

	start := time.Now()
	result, err := s.evaluator.Evaluate(r.Context(), data, req.Tags)
	metrics.RecordEvaluatorLatency(s.evaluatorImpl, start)
	if err != nil {
		slog.Error("tagger: evaluation failed", "error", err)
		http.Error(w, `{"error":{"code":"evaluation_error","message":"tag evaluation failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tagResponse{Tags: result})
}

func validateTag(tag string) error {
	if tag == "" {
		return fmt.Errorf("tag cannot be empty")
	}
	if !utf8.ValidString(tag) {
		return fmt.Errorf("tag must be valid UTF-8")
	}
	if len(tag) > 128 {
		return fmt.Errorf("tag exceeds 128 bytes")
	}
	return nil
}
