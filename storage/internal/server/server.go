package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/storage/internal/config"
	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/metrics"
	"mrsydar/tagona/storage/internal/models"
	"mrsydar/tagona/storage/internal/query"
	"mrsydar/tagona/storage/internal/storage"
	"mrsydar/tagona/storage/internal/validate"
	"mrsydar/tagona/storage/pkg/client"
)

// Server holds dependencies for the HTTP server.
type Server struct {
	cfg            *config.Config
	db             *db.DB
	store          *storage.S3Store
	tagClient      client.Tagger
	queryRunner    *query.Runner
	supportedTypes []string
}

// NewServer creates a new Server.
func NewServer(cfg *config.Config, database *db.DB, store *storage.S3Store, tagClient client.Tagger) *Server {
	return &Server{
		cfg:         cfg,
		db:          database,
		store:       store,
		tagClient:   tagClient,
		queryRunner: query.NewRunner(database, tagClient),
	}
}

// SetSupportedTypes sets the supported data types from tagging engine.
func (s *Server) SetSupportedTypes(types []string) {
	s.supportedTypes = types
}

// Router builds and returns the chi router.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	r.Get("/v1/collections", s.listCollections)
	r.Post("/v1/collections", s.createCollection)
	r.Delete("/v1/collections/{collection}", s.deleteCollection)
	r.Get("/v1/collections/{collection}/tags", s.listCollectionTags)
	r.Post("/v1/collections/{collection}/objects", s.putObject)
	r.Get("/v1/collections/{collection}/objects/{id}", s.getObjectMetadata)
	r.Get("/v1/collections/{collection}/objects/{id}/data", s.getObjectData)
	r.Get("/v1/collections/{collection}/objects/{id}/tags", s.getObjectTags)
	r.Post("/v1/collections/{collection}/objects/query", s.queryObjects)
	r.Delete("/v1/collections/{collection}/objects/{id}", s.deleteObject)

	return r
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	slog.Debug("healthz handler called")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	slog.Debug("readyz handler called")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	// Check DB
	slog.Debug("readyz checking database")
	if err := s.db.Pool().Ping(ctx); err != nil {
		slog.Error("readyz db ping failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database not available")
		return
	}
	// Check S3
	slog.Debug("readyz checking S3")
	if err := s.store.HeadBucket(ctx); err != nil {
		slog.Error("readyz s3 check failed", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "object storage not available")
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	slog.Debug("listCollections handler called")
	collections, err := s.db.ListCollections(r.Context())
	if err != nil {
		slog.Error("list collections failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list collections")
		return
	}
	resp := models.CollectionsListResponse{
		Collections: collections,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	slog.Debug("createCollection handler called")
	var req models.CollectionCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid request body")
		return
	}
	if err := validate.ValidateCollectionName(req.Name); err != nil {
		slog.Debug("createCollection validation failed: invalid name", "name", req.Name)
		writeError(w, http.StatusBadRequest, "invalid_name", err.Error())
		return
	}
	if err := validate.ValidateDataType(req.DataType, s.supportedTypes); err != nil {
		slog.Debug("createCollection validation failed: unsupported data type", "data_type", req.DataType)
		writeError(w, http.StatusBadRequest, "unsupported_data_type", err.Error())
		return
	}
	slog.Debug("creating collection", "name", req.Name, "data_type", req.DataType)
	coll, err := s.db.CreateCollection(r.Context(), req.Name, req.DataType)
	if err != nil {
		// Check for unique violation
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			writeError(w, http.StatusConflict, "already_exists", "collection already exists")
			return
		}
		slog.Error("create collection failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to create collection")
		return
	}
	slog.Debug("collection created", "name", coll.Name, "data_type", coll.DataType)
	resp := models.CollectionCreateResponse{
		Name:     coll.Name,
		DataType: coll.DataType,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

const (
	defaultTagsLimit = 100
	maxTagsLimit     = 1000
)

// listCollectionTags returns the number of objects in a collection and the tags
// registered in it, with per-tag counts. Tags are ordered by name; pass the
// returned next cursor to fetch the following page.
func (s *Server) listCollectionTags(w http.ResponseWriter, r *http.Request) {
	slog.Debug("listCollectionTags handler called")
	collectionName := chi.URLParam(r, "collection")
	if err := validate.ValidateCollectionName(collectionName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_collection_name", err.Error())
		return
	}

	q := r.URL.Query()
	limit := defaultTagsLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		if n > maxTagsLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit exceeds max of %d", maxTagsLimit))
			return
		}
		limit = n
	}
	prefix := q.Get("prefix")
	if len(prefix) > 128 || !utf8.ValidString(prefix) {
		writeError(w, http.StatusBadRequest, "invalid_prefix", "prefix must be valid UTF-8 of at most 128 bytes")
		return
	}
	afterTag := ""
	if c := q.Get("cursor"); c != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || !utf8.Valid(decoded) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
			return
		}
		afterTag = string(decoded)
	}

	coll, err := s.db.GetCollectionByName(r.Context(), collectionName)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}

	// Fetch one extra row to know whether another page exists.
	total, stats, err := s.db.GetCollectionTagStats(r.Context(), coll.ID, prefix, afterTag, limit+1)
	if err != nil {
		if strings.Contains(err.Error(), "collection not found") {
			writeError(w, http.StatusNotFound, "not_found", "collection not found")
			return
		}
		slog.Error("get collection tag stats failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to get collection tags")
		return
	}

	resp := models.CollectionTagsResponse{
		Collection:   coll.Name,
		TotalObjects: total,
		Tags:         stats,
	}
	if len(stats) > limit {
		resp.Tags = stats[:limit]
		resp.Next = base64.RawURLEncoding.EncodeToString([]byte(resp.Tags[limit-1].Tag))
	}
	if resp.Tags == nil {
		resp.Tags = []models.TagStat{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	slog.Debug("deleteCollection handler called")
	collectionName := chi.URLParam(r, "collection")
	if err := validate.ValidateCollectionName(collectionName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_collection_name", err.Error())
		return
	}

	coll, err := s.db.GetCollectionByName(r.Context(), collectionName)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}

	payloadKeys, err := s.db.GetCollectionPayloadKeys(r.Context(), coll.ID)
	if err != nil {
		slog.Error("get collection payload keys failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete collection")
		return
	}

	if err := s.db.DeleteCollection(r.Context(), coll.ID); err != nil {
		slog.Error("delete collection failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete collection")
		return
	}

	for _, key := range payloadKeys {
		if err := s.store.Delete(r.Context(), key); err != nil {
			slog.Warn("delete collection object from S3 failed", "error", err, "key", key)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request) {
	slog.Debug("putObject handler called")
	collectionName := chi.URLParam(r, "collection")
	if err := validate.ValidateCollectionName(collectionName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_collection_name", err.Error())
		return
	}

	coll, err := s.db.GetCollectionByName(r.Context(), collectionName)
	if err != nil {
		slog.Debug("putObject collection not found", "collection", collectionName)
		writeError(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}

	dataType := r.URL.Query().Get("data_type")
	if dataType == "" {
		slog.Debug("putObject missing data_type")
		writeError(w, http.StatusBadRequest, "missing_data_type", "data_type is required")
		return
	}
	if dataType != coll.DataType {
		slog.Debug("putObject data_type mismatch", "collection_type", coll.DataType, "object_type", dataType)
		writeError(w, http.StatusBadRequest, "invalid_data_type", "object data_type does not match collection")
		return
	}

	ttlSecondsStr := r.URL.Query().Get("ttl_seconds")
	var expiresAt *time.Time
	if ttlSecondsStr != "" {
		ttlSec, err := strconv.Atoi(ttlSecondsStr)
		if err != nil || ttlSec < 0 {
			writeError(w, http.StatusBadRequest, "invalid_ttl", "invalid ttl_seconds")
			return
		}
		t := time.Now().UTC().Add(time.Duration(ttlSec) * time.Second)
		expiresAt = &t
	} else if s.cfg.DefaultTTL > 0 {
		t := time.Now().UTC().Add(s.cfg.DefaultTTL)
		expiresAt = &t
	}

	dateStr := r.URL.Query().Get("date")
	var date time.Time
	if dateStr != "" {
		var err error
		date, err = time.Parse(time.RFC3339, dateStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_date", "invalid date format")
			return
		}
	} else {
		date = time.Now().UTC()
	}

	slog.Debug("putObject buffering upload to temp file")
	tmpFile, err := os.CreateTemp("", "tagona-upload-*")
	if err != nil {
		slog.Error("create temp file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to buffer upload")
		return
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
	}()

	hasher := sha256.New()
	limited := io.LimitReader(r.Body, s.cfg.MaxObjectSizeBytes+1)
	written, err := io.Copy(io.MultiWriter(tmpFile, hasher), limited)
	if err != nil {
		slog.Error("stream body failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to read body")
		return
	}
	if written > s.cfg.MaxObjectSizeBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("payload exceeds %d bytes", s.cfg.MaxObjectSizeBytes))
		return
	}

	contentHash := hex.EncodeToString(hasher.Sum(nil))

	slog.Debug("putObject checking for duplicate hash", "hash", contentHash)
	// Check for existing object in collection.
	if existing, err := s.db.GetObjectByCollectionAndHash(r.Context(), coll.ID, contentHash); err == nil {
		slog.Debug("putObject found duplicate object", "id", existing.ID)
		// Delete newly uploaded S3 object if it exists (in case of race). We haven't uploaded yet though.
		resp := models.ObjectUploadResponse{
			ID:          existing.ID,
			Collection:  existing.Collection,
			DataType:    existing.DataType,
			Date:        existing.Date,
			SizeBytes:   existing.SizeBytes,
			ContentHash: existing.ContentHash,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
		return
	}

	slog.Debug("putObject inserting into database")
	// Insert object first so we have an ID, then upload to S3.
	objectID := ""
	// We need to insert to get ID, but if S3 fails, we need to clean up.
	// Alternative: generate UUID client-side or use DB insert first.
	// Let's insert first.
	payloadKey := fmt.Sprintf("%s/%s", collectionName, contentHash) // temporary, will update with actual ID
	// Actually, per design: payload_key format: <collection>/<object_id>
	// Since we need object_id, let's use UUID generated by postgres.
	// We'll insert with a dummy payload_key, then update after we get ID.
	obj, err := s.db.InsertObject(r.Context(), coll.ID, contentHash, date, written, dataType, "temp", expiresAt)
	if err != nil {
		// Handle duplicate caused by concurrent insert or expired row.
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			existing, err2 := s.db.GetObjectByCollectionAndHashIncludingExpired(r.Context(), coll.ID, contentHash)
			if err2 == nil {
				if existing.ExpiresAt != nil && !existing.ExpiresAt.After(time.Now().UTC()) {
					payloadKey, delErr := s.db.DeleteObject(r.Context(), existing.ID)
					if delErr != nil {
						slog.Error("delete expired duplicate failed", "error", delErr)
						writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete expired duplicate")
						return
					}
					if err := s.store.Delete(r.Context(), payloadKey); err != nil {
						slog.Warn("delete expired duplicate from s3 failed", "error", err, "key", payloadKey)
					}
					obj, err = s.db.InsertObject(r.Context(), coll.ID, contentHash, date, written, dataType, "temp", expiresAt)
					if err != nil {
						slog.Error("insert object after cleanup failed", "error", err)
						writeError(w, http.StatusInternalServerError, "internal_error", "failed to insert object")
						return
					}
				} else {
					resp := models.ObjectUploadResponse{
						ID:          existing.ID,
						Collection:  existing.Collection,
						DataType:    existing.DataType,
						Date:        existing.Date,
						SizeBytes:   existing.SizeBytes,
						ContentHash: existing.ContentHash,
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(resp)
					return
				}
			}
		}
		slog.Error("insert object failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to insert object")
		return
	}
	objectID = obj.ID
	payloadKey = fmt.Sprintf("%s/%s", collectionName, objectID)

	slog.Debug("putObject updating payload_key", "objectID", objectID, "payloadKey", payloadKey)
	// Update payload_key.
	_, err = s.db.Pool().Exec(r.Context(), `UPDATE objects SET payload_key = $1 WHERE id = $2`, payloadKey, objectID)
	if err != nil {
		slog.Error("update payload key failed", "error", err)
		// Best effort cleanup
		s.db.Pool().Exec(r.Context(), `DELETE FROM objects WHERE id = $1`, objectID)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to update object")
		return
	}

	slog.Debug("putObject uploading to S3")
	// Upload to S3.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		slog.Error("seek temp file failed", "error", err)
		s.db.Pool().Exec(r.Context(), `DELETE FROM objects WHERE id = $1`, objectID)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store payload")
		return
	}
	if err := s.store.Upload(r.Context(), payloadKey, tmpFile, written); err != nil {
		slog.Error("s3 upload failed", "error", err)
		s.db.Pool().Exec(r.Context(), `DELETE FROM objects WHERE id = $1`, objectID)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to store payload")
		return
	}

	slog.Debug("putObject upload complete", "id", objectID, "size", written)
	resp := models.ObjectUploadResponse{
		ID:          objectID,
		Collection:  collectionName,
		DataType:    dataType,
		Date:        date,
		SizeBytes:   written,
		ContentHash: contentHash,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) getObjectMetadata(w http.ResponseWriter, r *http.Request) {
	slog.Debug("getObjectMetadata handler called")
	id := chi.URLParam(r, "id")
	obj, err := s.db.GetObjectByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	// Verify collection.
	collectionName := chi.URLParam(r, "collection")
	if obj.Collection != collectionName {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(obj)
}

func (s *Server) getObjectData(w http.ResponseWriter, r *http.Request) {
	slog.Debug("getObjectData handler called")
	id := chi.URLParam(r, "id")
	obj, err := s.db.GetObjectByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	collectionName := chi.URLParam(r, "collection")
	if obj.Collection != collectionName {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}

	reader, size, err := s.store.Download(r.Context(), obj.PayloadKey)
	if err != nil {
		slog.Error("download failed", "error", err, "key", obj.PayloadKey)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to retrieve payload")
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	io.Copy(w, reader)
}

func (s *Server) getObjectTags(w http.ResponseWriter, r *http.Request) {
	slog.Debug("getObjectTags handler called")
	// evaluate=false returns only already-known tags (null for requested tags
	// that were never evaluated) and never calls the tagging engine.
	evaluate := true
	if v := r.URL.Query().Get("evaluate"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_evaluate", "evaluate must be true or false")
			return
		}
		evaluate = b
	}

	id := chi.URLParam(r, "id")
	obj, err := s.db.GetObjectByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	collectionName := chi.URLParam(r, "collection")
	if obj.Collection != collectionName {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}

	tagsParam := r.URL.Query().Get("tags")
	var requestedTags []string
	seenTags := map[string]struct{}{}
	if tagsParam != "" {
		for _, t := range strings.Split(tagsParam, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				if _, exists := seenTags[t]; exists {
					continue
				}
				seenTags[t] = struct{}{}
				requestedTags = append(requestedTags, t)
			}
		}
	}
	if len(requestedTags) > s.cfg.MaxTagsPerQuery {
		writeError(w, http.StatusBadRequest, "invalid_tags", "too many tags requested")
		return
	}
	for _, tag := range requestedTags {
		if err := validate.ValidateTag(tag); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tag", err.Error())
			return
		}
	}

	knownTags, err := s.db.GetTagsForObject(r.Context(), id)
	if err != nil {
		slog.Error("get tags failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to get tags")
		return
	}

	slog.Debug("getObjectTags evaluating missing tags", "requested_count", len(requestedTags), "evaluate", evaluate)
	// Evaluate missing tags if requestedTags provided.
	if evaluate && len(requestedTags) > 0 {
		var missing []string
		for _, tag := range requestedTags {
			if _, has := knownTags[tag]; !has {
				missing = append(missing, tag)
			}
		}
		if len(missing) > 0 {
			// Call tagging engine.
			resp, err := s.tagClient.Tag(r.Context(), collectionName, id, missing)
			if err != nil {
				slog.Error("tag engine failed", "error", err)
				writeError(w, http.StatusBadGateway, "tag_engine_error", "tag engine failed")
				return
			}
			if err := s.db.UpsertTags(r.Context(), obj.CollectionID, id, resp); err != nil {
				slog.Error("upsert tags failed", "error", err)
				writeError(w, http.StatusInternalServerError, "internal_error", "failed to persist tags")
				return
			}
			for k, v := range resp {
				knownTags[k] = v
			}
		}
	}

	// Values are pointers so a requested-but-unevaluated tag can be null.
	result := map[string]*bool{}
	if len(requestedTags) > 0 {
		for _, tag := range requestedTags {
			if v, has := knownTags[tag]; has {
				val := v
				result[tag] = &val
			} else if !evaluate {
				result[tag] = nil
			}
		}
	} else {
		for tag, v := range knownTags {
			val := v
			result[tag] = &val
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":   id,
		"tags": result,
	})
}

const (
	minQueryTimeoutMs     = 1000   // 1s
	maxQueryTimeoutMs     = 300000 // 5m
	defaultQueryTimeoutMs = 30000  // 30s
)

func (s *Server) queryObjects(w http.ResponseWriter, r *http.Request) {
	slog.Debug("queryObjects handler called")
	collectionName := chi.URLParam(r, "collection")
	if err := validate.ValidateCollectionName(collectionName); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_collection_name", err.Error())
		return
	}

	coll, err := s.db.GetCollectionByName(r.Context(), collectionName)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "collection not found")
		return
	}

	var req models.TagsQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid request body")
		return
	}

	if req.Limit <= 0 {
		req.Limit = s.cfg.DefaultLimit
	}
	if req.Limit > s.cfg.MaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit exceeds max")
		return
	}

	if req.Tags == nil {
		req.Tags = map[string]bool{}
	}

	if err := validate.ValidateTags(req.Tags, s.cfg.MaxTagsPerQuery); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_tags", err.Error())
		return
	}
	if err := validate.ValidateDateFilter(req.Date); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_date_filter", err.Error())
		return
	}

	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = defaultQueryTimeoutMs
	} else {
		if timeoutMs < minQueryTimeoutMs {
			writeError(w, http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeout_ms must be at least %dms", minQueryTimeoutMs))
			return
		}
		if timeoutMs > maxQueryTimeoutMs {
			writeError(w, http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeout_ms must be at most %dms", maxQueryTimeoutMs))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	resp, err := s.queryRunner.Query(ctx, coll, req)
	if err != nil {
		slog.Error("query failed", "error", err)
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			writeError(w, http.StatusInternalServerError, "query_timeout", "query timed out")
			return
		}
		if strings.Contains(err.Error(), "tag engine error") || strings.Contains(err.Error(), "tag engine failure") {
			writeError(w, http.StatusBadGateway, "tag_engine_error", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "query failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request) {
	slog.Debug("deleteObject handler called")
	id := chi.URLParam(r, "id")
	collectionName := chi.URLParam(r, "collection")
	obj, err := s.db.GetObjectByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if obj.Collection != collectionName {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}

	payloadKey, err := s.db.DeleteObject(r.Context(), id)
	if err != nil {
		slog.Error("delete object failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to delete object")
		return
	}

	if err := s.store.Delete(r.Context(), payloadKey); err != nil {
		slog.Warn("delete from S3 failed", "error", err, "key", payloadKey)
	}

	w.WriteHeader(http.StatusNoContent)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := models.ErrorResponse{}
	resp.Error.Code = code
	resp.Error.Message = message
	json.NewEncoder(w).Encode(resp)
}

func requestLogger() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"duration", time.Since(start),
			)
		})
	}
}
