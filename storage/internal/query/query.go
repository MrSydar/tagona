package query

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"mrsydar/tagona/storage/internal/cursor"
	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/models"
	"mrsydar/tagona/storage/pkg/client"
)

// Runner executes tag queries.
type Runner struct {
	db     *db.DB
	client client.Tagger
}

// NewRunner creates a new query runner.
func NewRunner(database *db.DB, client client.Tagger) *Runner {
	slog.Debug("NewRunner: created")
	return &Runner{db: database, client: client}
}

// Query executes a tag query.
func (r *Runner) Query(ctx context.Context, collection *models.Collection, req models.TagsQueryRequest) (*models.TagsQueryResponse, error) {
	slog.Debug("Query", "collection", collection.Name, "limit", req.Limit, "tags", len(req.Tags))
	var cursorDate time.Time
	var cursorID string
	if req.Cursor != "" {
		var err error
		cursorDate, cursorID, err = cursor.Decode(req.Cursor)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor: %w", err)
		}
	}

	targetLimit := req.Limit + 1

	// No tag filtering: return by date only.
	if len(req.Tags) == 0 {
		slog.Debug("Query: no tags provided, querying by date only")
		objs, err := r.db.ScanCandidateObjects(ctx, collection.ID, req.Date, cursorDate, cursorID, targetLimit)
		if err != nil {
			return nil, fmt.Errorf("query by date: %w", err)
		}
		return buildResponse(objs, req.Limit)
	}

	results := make([]models.Object, 0, targetLimit)
	scanCursorDate := cursorDate
	scanCursorID := cursorID

	batchSize := req.Limit*5 + 100
	if batchSize < 200 {
		batchSize = 200
	}

	slog.Debug("Query: starting batch scan for tags")
	for len(results) < targetLimit {
		if err := ctx.Err(); err != nil {
			if req.BestEffort {
				return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
			}
			return nil, fmt.Errorf("query timeout: %w", err)
		}
		candidates, err := r.db.ScanCandidateObjects(ctx, collection.ID, req.Date, scanCursorDate, scanCursorID, batchSize)
		if err != nil {
			if req.BestEffort && ctx.Err() != nil {
				return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
			}
			return nil, fmt.Errorf("scan candidates: %w", err)
		}
		if len(candidates) == 0 {
			break
		}

		candidateIDs := make([]string, len(candidates))
		for i, c := range candidates {
			candidateIDs[i] = c.ID
		}
		knownTags, err := r.db.GetKnownTagsForObjects(ctx, candidateIDs)
		if err != nil {
			if req.BestEffort && ctx.Err() != nil {
				return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
			}
			return nil, fmt.Errorf("get known tags: %w", err)
		}

		for _, cand := range candidates {
			if len(results) >= targetLimit {
				break
			}
			if err := ctx.Err(); err != nil {
				if req.BestEffort {
					return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
				}
				return nil, fmt.Errorf("query timeout: %w", err)
			}
			objKnown := knownTags[cand.ID]
			contradicts := false
			for tag, wanted := range req.Tags {
				if val, has := objKnown[tag]; has && val != wanted {
					contradicts = true
					break
				}
			}

			scanCursorDate = cand.Date
			scanCursorID = cand.ID

			if contradicts {
				continue
			}

			var missing []string
			for tag := range req.Tags {
				if _, has := objKnown[tag]; !has {
					missing = append(missing, tag)
				}
			}
			if len(missing) > 0 {
				// Call tagging engine.
				resp, err := r.client.Tag(ctx, collection.Name, cand.ID, missing)
				if err != nil {
					if req.BestEffort && ctx.Err() != nil {
						return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
					}
					return nil, fmt.Errorf("tag engine error: %w", err)
				}
				if err := r.db.UpsertTags(ctx, collection.ID, cand.ID, resp); err != nil {
					if req.BestEffort && ctx.Err() != nil {
						return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
					}
					return nil, fmt.Errorf("persist tags: %w", err)
				}
				if objKnown == nil {
					objKnown = map[string]bool{}
				}
				for k, v := range resp {
					objKnown[k] = v
				}
			}

			matches := true
			for tag, wanted := range req.Tags {
				val, has := objKnown[tag]
				if !has {
					matches = false
					break
				}
				if val != wanted {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			cand.Tags = req.Tags
			results = append(results, cand)
		}
	}

	return buildResponse(results, req.Limit)
}

func buildResponse(results []models.Object, limit int) (*models.TagsQueryResponse, error) {
	slog.Debug("buildResponse", "results", len(results), "limit", limit)
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}
	resp := &models.TagsQueryResponse{
		Objects: results,
	}
	if hasMore && len(results) > 0 {
		last := results[len(results)-1]
		resp.Next = cursor.Encode(last.Date, last.ID)
	}
	return resp, nil
}

func buildPartialResponse(results []models.Object, limit int, scanCursorDate time.Time, scanCursorID string) (*models.TagsQueryResponse, error) {
	slog.Debug("buildPartialResponse", "results", len(results), "limit", limit, "scanCursorID", scanCursorID)
	if len(results) > limit {
		return buildResponse(results, limit)
	}
	resp := &models.TagsQueryResponse{
		Objects: results,
	}
	if scanCursorID != "" {
		resp.Next = cursor.Encode(scanCursorDate, scanCursorID)
	}
	return resp, nil
}
