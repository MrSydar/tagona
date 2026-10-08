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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"mrsydar/tagona/storage/internal/config"
	"mrsydar/tagona/storage/internal/cursor"
	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/metrics"
	"mrsydar/tagona/storage/internal/models"
	"mrsydar/tagona/storage/internal/query"
	"mrsydar/tagona/storage/internal/storage"
	"mrsydar/tagona/storage/internal/validate"
	"mrsydar/tagona/storage/pkg/client"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Server holds dependencies for the HTTP server.
type Server struct {
	cfg         *config.Config
	db          *db.DB
	store       *storage.S3Store
	tagClient   client.Tagger
	queryRunner *query.Runner
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

// Router builds and returns the chi router.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	r.Use(requestLogger())
	r.Use(metrics.Middleware)

	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	r.Get("/collections", s.listCollections)
	r.Post("/collections", s.createCollection)
	r.Delete("/collections/{collection}", s.deleteCollection)
	r.Get("/taggers", s.listTaggers)
	r.Get("/collections/{collection}/tags", s.listCollectionTags)
	r.Post("/collections/{collection}/objects", s.putObject)
	r.Get("/collections/{collection}/objects/{id}", s.getObjectMetadata)
	r.Get("/collections/{collection}/objects/{id}/data", s.getObjectData)
	r.Get("/collections/{collection}/objects/{id}/tags", s.getObjectTags)
	r.Put("/collections/{collection}/objects/{id}/metadata", s.replaceMetadata)
	r.Patch("/collections/{collection}/objects/{id}/metadata", s.mergeMetadata)
	r.Post("/collections/{collection}/objects/query", s.queryObjects)
	r.Delete("/collections/{collection}/objects/{id}", s.deleteObject)

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

// listTaggers lists the tagger versions available for new collections: the versions the tagging engine
// reports at its /version.
func (s *Server) listTaggers(w http.ResponseWriter, r *http.Request) {
	versions, err := s.tagClient.Versions(r.Context())
	if err != nil {
		slog.Error("fetching the tagger versions failed", "error", err)
		writeError(w, http.StatusBadGateway, "tag_engine_error", "the tagging engine's versions could not be fetched")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string][]string{"taggers": versions})
}

// Lists are paged: limit defaults to defaultListLimit and may not exceed maxListLimit; the cursor is the
// opaque next value of the previous page.
const (
	defaultListLimit = 100
	maxListLimit     = 1000
)

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	slog.Debug("listCollections handler called")
	q := r.URL.Query()
	limit := defaultListLimit
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxListLimit {
			writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be an integer from 1 to %d", maxListLimit))
			return
		}
		limit = n
	}
	var after *db.CollectionCursor
	if q.Has("cursor") {
		created, id, err := cursor.DecodeKey(q.Get("cursor"))
		if err != nil || !uuidRe.MatchString(id) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is not valid")
			return
		}
		after = &db.CollectionCursor{CreatedAt: created, ID: id}
	}

	collections, more, err := s.db.ListCollections(r.Context(), limit, after)
	if err != nil {
		slog.Error("list collections failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to list collections")
		return
	}
	resp := models.CollectionsListResponse{Collections: collections}
	if more && len(collections) > 0 {
		last := collections[len(collections)-1]
		resp.Next = cursor.EncodeKey(last.CreatedAt, last.ID)
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
	if req.TaggerVersion == "" {
		// Not given: the collection is tagged with the tagger that runs now, if there is only one.
		versions, err := s.tagClient.Versions(r.Context())
		if err != nil {
			slog.Error("fetching the tagger versions failed", "error", err)
			writeError(w, http.StatusBadGateway, "tag_engine_error", "the tagger version could not be determined: give tagger_version")
			return
		}
		if len(versions) != 1 {
			writeErrorDetails(w, http.StatusBadRequest, "tagger_version_required",
				"the tagging engine serves several versions: give tagger_version", map[string]any{"available": versions})
			return
		}
		req.TaggerVersion = versions[0]
	}
	if err := validate.ValidateTaggerVersion(req.TaggerVersion); err != nil {
		slog.Debug("createCollection validation failed: invalid tagger version", "tagger_version", req.TaggerVersion)
		writeError(w, http.StatusBadRequest, "invalid_tagger_version", err.Error())
		return
	}
	slog.Debug("creating collection", "name", req.Name, "tagger_version", req.TaggerVersion)
	coll, err := s.db.CreateCollection(r.Context(), req.Name, req.TaggerVersion)
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
	slog.Debug("collection created", "name", coll.Name, "tagger_version", coll.TaggerVersion)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(coll)
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

	metadata, err := parseMetadataParam(r.URL.Query().Get("metadata"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
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
			Metadata:    existing.Metadata,
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
	obj, err := s.db.InsertObject(r.Context(), coll.ID, contentHash, date, written, "temp", metadata, expiresAt)
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
					obj, err = s.db.InsertObject(r.Context(), coll.ID, contentHash, date, written, "temp", metadata, expiresAt)
					if err != nil {
						slog.Error("insert object after cleanup failed", "error", err)
						writeError(w, http.StatusInternalServerError, "internal_error", "failed to insert object")
						return
					}
				} else {
					resp := models.ObjectUploadResponse{
						ID:          existing.ID,
						Collection:  existing.Collection,
						Metadata:    existing.Metadata,
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
		Metadata:    metadata,
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
			coll, err := s.db.GetCollectionByName(r.Context(), collectionName)
			if err != nil {
				writeError(w, http.StatusNotFound, "not_found", "collection not found")
				return
			}
			// Call tagging engine, which must be the version the collection is tagged with.
			resp, err := s.tagClient.Tag(r.Context(), collectionName, id, coll.TaggerVersion, missing)
			if err != nil {
				if writeTaggerError(w, err) {
					return
				}
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
		if writeTaggerError(w, err) {
			return
		}
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
	writeErrorDetails(w, status, code, message, nil)
}

func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := models.ErrorResponse{}
	resp.Error.Code = code
	resp.Error.Message = message
	resp.Error.Details = details
	json.NewEncoder(w).Encode(resp)
}

// writeTaggerError answers 409 tagger_version_mismatch when err says the tagging engine does not serve the
// version the collection is tagged with, and reports whether it did. Tags the collection already has
// stay usable: a request with evaluate=false never reaches the engine.
func writeTaggerError(w http.ResponseWriter, err error) bool {
	var mismatch *client.VersionMismatchError
	if !errors.As(err, &mismatch) {
		return false
	}
	writeErrorDetails(w, http.StatusConflict, "tagger_version_mismatch",
		fmt.Sprintf("the collection is tagged with %q but the tagging engine serves %s; query with evaluate=false to use the tags it has", mismatch.Expected, listOf(mismatch.Running)),
		map[string]any{"expected": mismatch.Expected, "running": mismatch.Running})
	return true
}

// listOf writes a list of versions for a message: ["grep","false"].
func listOf(items []string) string {
	b, _ := json.Marshal(items)
	return string(b)
}

// parseMetadataParam reads the metadata query parameter of an upload: a JSON object of string values, or
// nothing.
func parseMetadataParam(raw string) (map[string]string, error) {
	if raw == "" {
		return map[string]string{}, nil
	}
	m := map[string]string{}
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&m); err != nil || dec.More() {
		return nil, errors.New("metadata must be a JSON object whose values are strings")
	}
	if m == nil { // the JSON null
		m = map[string]string{}
	}
	if err := validate.ValidateMetadata(m); err != nil {
		return nil, err
	}
	return m, nil
}

const maxMetadataBodyBytes = 16 << 10

// objectOfRequest finds the object a metadata request is about, or answers 404.
func (s *Server) objectOfRequest(w http.ResponseWriter, r *http.Request) (*models.Object, bool) {
	obj, err := s.db.GetObjectByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || obj.Collection != chi.URLParam(r, "collection") {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return nil, false
	}
	return obj, true
}

// replaceMetadata (PUT) replaces all metadata of an object with the body, a JSON object of string values.
func (s *Server) replaceMetadata(w http.ResponseWriter, r *http.Request) {
	obj, ok := s.objectOfRequest(w, r)
	if !ok {
		return
	}
	var body map[string]string
	dec := json.NewDecoder(io.LimitReader(r.Body, maxMetadataBodyBytes))
	if err := dec.Decode(&body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "the body must be a JSON object whose values are strings")
		return
	}
	if err := validate.ValidateMetadata(body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_metadata", err.Error())
		return
	}
	s.changeMetadata(w, r, obj, func(map[string]string) (map[string]string, error) { return body, nil })
}

// mergeMetadata (PATCH) sets the keys of the body, a JSON object, and removes those whose value is null; the
// other keys stay.
func (s *Server) mergeMetadata(w http.ResponseWriter, r *http.Request) {
	obj, ok := s.objectOfRequest(w, r)
	if !ok {
		return
	}
	var body map[string]*string
	dec := json.NewDecoder(io.LimitReader(r.Body, maxMetadataBodyBytes))
	if err := dec.Decode(&body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "the body must be a JSON object whose values are strings or null")
		return
	}
	s.changeMetadata(w, r, obj, func(current map[string]string) (map[string]string, error) {
		merged := make(map[string]string, len(current)+len(body))
		for k, v := range current {
			merged[k] = v
		}
		for k, v := range body {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = *v
			}
		}
		if err := validate.ValidateMetadata(merged); err != nil {
			return nil, &metadataError{err}
		}
		return merged, nil
	})
}

// metadataError marks a validation failure of the merged metadata.
type metadataError struct{ error }

// changeMetadata applies change to the object's metadata and answers with the object as it is now.
func (s *Server) changeMetadata(w http.ResponseWriter, r *http.Request, obj *models.Object, change func(map[string]string) (map[string]string, error)) {
	if _, err := s.db.UpdateObjectMetadata(r.Context(), obj.ID, change); err != nil {
		var invalid *metadataError
		switch {
		case errors.As(err, &invalid):
			writeError(w, http.StatusBadRequest, "invalid_metadata", invalid.Error())
		case errors.Is(err, db.ErrObjectNotFound):
			writeError(w, http.StatusNotFound, "not_found", "object not found")
		default:
			slog.Error("update metadata failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "failed to update metadata")
		}
		return
	}
	updated, err := s.db.GetObjectByID(r.Context(), obj.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
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
