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
}

// NewServer creates a new tagging engine server.
func NewServer(storageClient *storageclient.Client, evaluator evaluator.Evaluator, evaluatorImpl string) *Server {
	return &Server{
		storage:       storageClient,
		evaluator:     evaluator,
		evaluatorImpl: evaluatorImpl,
	}
}

// Router builds the chi router.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Use(metrics.Middleware)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/v1/supported-types", s.supportedTypes)
	r.Post("/v1/tag", s.tag)
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

func (s *Server) supportedTypes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"types": s.evaluator.GetSupportedDataTypes(),
	})
}

type tagRequest struct {
	Collection string   `json:"collection"`
	ObjectID   string   `json:"object_id"`
	Tags       []string `json:"tags"`
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

	if req.Collection == "" || req.ObjectID == "" {
		http.Error(w, `{"error":{"code":"invalid_request","message":"collection and object_id required"}}`, http.StatusBadRequest)
		return
	}
	for _, tag := range req.Tags {
		if err := validateTag(tag); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":{"code":"invalid_tag","message":"%s"}}`, err.Error()), http.StatusBadRequest)
			return
		}
	}

	// Fetch object metadata from storage service first.
	meta, err := s.storage.GetObjectMetadata(r.Context(), req.Collection, req.ObjectID)
	if err != nil {
		slog.Error("tagger: fetch metadata failed", "error", err)
		http.Error(w, `{"error":{"code":"storage_error","message":"failed to fetch metadata"}}`, http.StatusBadGateway)
		return
	}

	// Fetch object data.
	data, err := s.storage.GetObjectData(r.Context(), req.Collection, req.ObjectID)
	if err != nil {
		slog.Error("tagger: fetch data failed", "error", err)
		http.Error(w, `{"error":{"code":"storage_error","message":"failed to fetch data"}}`, http.StatusBadGateway)
		return
	}

	start := time.Now()
	result, err := s.evaluator.Evaluate(r.Context(), evaluator.DataType(meta.DataType), data, req.Tags)
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
