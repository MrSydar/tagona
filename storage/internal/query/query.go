package query

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"mrsydar/tagona/storage/internal/cursor"
	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/models"
	"mrsydar/tagona/storage/pkg/client"
)

// Runner executes tag queries.
type Runner struct {
	db          *db.DB
	client      client.Tagger
	concurrency int
}

// NewRunner creates a new query runner. concurrency is how many objects a query has the tagger evaluate at
// once (1: one after the other).
func NewRunner(database *db.DB, client client.Tagger, concurrency int) *Runner {
	slog.Debug("NewRunner: created", "concurrency", concurrency)
	return &Runner{db: database, client: client, concurrency: max(1, concurrency)}
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

	// evaluate=false: answer from tags that are already known, without calling
	// the tagging engine.
	if !req.ShouldEvaluate() {
		return r.queryKnown(ctx, collection, req, cursorDate, cursorID, targetLimit)
	}

	// No tag filtering: return by date only.
	if len(req.Tags) == 0 {
		slog.Debug("Query: no tags provided, querying by date only")
		objs, err := r.db.ScanCandidateObjects(ctx, collection.ID, req.Date, cursorDate, cursorID, targetLimit)
		if err != nil {
			return nil, fmt.Errorf("query by date: %w", err)
		}
		return buildResponse(objs, req.Limit)
	}

	// The tagger is asked about several objects at once: evaluations run ahead of the scan, at most
	// r.concurrency of them in flight, while the answers are taken strictly in the order of the scan. So the
	// results, the early stop at limit+1 matches and the cursors are what a sequential scan gives; only the
	// waiting overlaps. Objects after the last match that were already started may be evaluated for nothing
	// (fewer than r.concurrency of them); what finishes is stored all the same.
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel() // whatever is still in flight is not needed
		wg.Wait()
	}()

	results := make([]models.Object, 0, targetLimit)
	// The scan cursor is the last object that is fully dealt with: a partial answer (a timeout) resumes
	// after it, so an object that was being evaluated is looked at again, not skipped.
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
		objects, err := r.db.ScanCandidateObjects(ctx, collection.ID, req.Date, scanCursorDate, scanCursorID, batchSize)
		if err != nil {
			if req.BestEffort && ctx.Err() != nil {
				return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
			}
			return nil, fmt.Errorf("scan candidates: %w", err)
		}
		if len(objects) == 0 {
			break
		}

		candidateIDs := make([]string, len(objects))
		for i, c := range objects {
			candidateIDs[i] = c.ID
		}
		knownTags, err := r.db.GetKnownTagsForObjects(ctx, candidateIDs)
		if err != nil {
			if req.BestEffort && ctx.Err() != nil {
				return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
			}
			return nil, fmt.Errorf("get known tags: %w", err)
		}

		items := make([]*candidate, len(objects))
		for i, obj := range objects {
			items[i] = plan(obj, knownTags[obj.ID], req.Tags)
		}

		next, inflight := 0, 0 // the first item not started, and the started ones that are not taken yet
		start := func() {
			for next < len(items) && inflight < r.concurrency {
				it := items[next]
				next++
				if it.missing == nil {
					continue
				}
				inflight++
				it.done = make(chan struct{})
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer close(it.done)
					r.evaluate(ctx, collection, it)
				}()
			}
		}

		for _, it := range items {
			if len(results) >= targetLimit {
				break
			}
			if err := ctx.Err(); err != nil {
				if req.BestEffort {
					return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
				}
				return nil, fmt.Errorf("query timeout: %w", err)
			}
			start()
			if it.done != nil {
				select {
				case <-it.done:
					inflight--
				case <-ctx.Done():
					if req.BestEffort {
						return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
					}
					return nil, fmt.Errorf("query timeout: %w", ctx.Err())
				}
				if it.err != nil {
					if req.BestEffort && ctx.Err() != nil {
						return buildPartialResponse(results, req.Limit, scanCursorDate, scanCursorID)
					}
					return nil, it.err
				}
				if it.known == nil {
					it.known = map[string]bool{}
				}
				for k, v := range it.tags {
					it.known[k] = v
				}
			}

			scanCursorDate = it.obj.Date
			scanCursorID = it.obj.ID
			if it.skip || !matchesAll(it.known, req.Tags) {
				continue
			}
			it.obj.Tags = req.Tags
			results = append(results, it.obj)
		}
	}

	return buildResponse(results, req.Limit)
}

// candidate is an object of the scan and what is known about it for the query.
type candidate struct {
	obj     models.Object
	known   map[string]bool
	skip    bool     // a known tag contradicts the query: it cannot match
	missing []string // tags to evaluate; nil when there is nothing to ask the tagger
	// Set when an evaluation was started: done is closed when it has finished, tags and err are its outcome.
	done chan struct{}
	tags map[string]bool
	err  error
}

// plan decides what a query needs to do for an object, from the tags already known for it.
func plan(obj models.Object, known map[string]bool, wanted map[string]bool) *candidate {
	c := &candidate{obj: obj, known: known}
	for tag, want := range wanted {
		have, ok := known[tag]
		switch {
		case !ok:
			c.missing = append(c.missing, tag)
		case have != want:
			c.skip = true
		}
	}
	if c.skip {
		c.missing = nil
	}
	return c
}

// matchesAll reports whether every wanted tag is known with the value wanted.
func matchesAll(known, wanted map[string]bool) bool {
	for tag, want := range wanted {
		if have, ok := known[tag]; !ok || have != want {
			return false
		}
	}
	return true
}

// evaluate asks the tagger for the missing tags of one object and stores the answer, so that it is kept even
// when the query no longer needs it.
func (r *Runner) evaluate(ctx context.Context, collection *models.Collection, c *candidate) {
	resp, err := r.client.Tag(ctx, collection.Name, c.obj.ID, collection.TaggerVersion, c.missing)
	if err != nil {
		c.err = fmt.Errorf("tag engine error: %w", err)
		return
	}
	if err := r.db.UpsertTags(ctx, collection.ID, c.obj.ID, resp); err != nil {
		c.err = fmt.Errorf("persist tags: %w", err)
		return
	}
	c.tags = resp
}

// queryKnown returns objects for which every requested tag is already known
// and matches. Objects with a requested tag that was never evaluated are not
// returned, since they cannot be confirmed to match. best_effort has no effect
// here: the lookup is a single indexed query, so there is no partial result.
func (r *Runner) queryKnown(ctx context.Context, collection *models.Collection, req models.TagsQueryRequest, cursorDate time.Time, cursorID string, targetLimit int) (*models.TagsQueryResponse, error) {
	slog.Debug("queryKnown", "collection", collection.Name, "tags", len(req.Tags))
	objs, err := r.db.QueryObjectsKnownTags(ctx, collection.ID, req.Tags, req.Date, cursorDate, cursorID, targetLimit)
	if err != nil {
		return nil, fmt.Errorf("query known tags: %w", err)
	}
	if objs == nil {
		objs = []models.Object{} // encode as [] like the evaluating path, not null
	}
	if len(req.Tags) > 0 {
		for i := range objs {
			objs[i].Tags = req.Tags
		}
	}
	return buildResponse(objs, req.Limit)
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
