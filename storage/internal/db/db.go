package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"mrsydar/tagona/storage/internal/models"
)

// DB wraps pgxpool and provides typed queries.
type DB struct {
	pool *pgxpool.Pool
}

// New creates a new DB instance.
func New(pool *pgxpool.Pool) *DB {
	slog.Debug("New DB instance created")
	return &DB{pool: pool}
}

// Close closes the pool.
func (d *DB) Close() {
	slog.Debug("Close DB pool")
	d.pool.Close()
}

// Pool returns the underlying pool.
func (d *DB) Pool() *pgxpool.Pool {
	slog.Debug("Pool: returning underlying pool")
	return d.pool
}

// CreateCollection inserts a collection.
func (d *DB) CreateCollection(ctx context.Context, name, taggerVersion string) (*models.Collection, error) {
	slog.Debug("CreateCollection", "name", name, "taggerVersion", taggerVersion)
	var c models.Collection
	err := d.pool.QueryRow(ctx,
		`INSERT INTO collections (name, tagger_version) VALUES ($1, $2) RETURNING id, name, tagger_version, created_at`,
		name, taggerVersion,
	).Scan(&c.ID, &c.Name, &c.TaggerVersion, &c.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert collection: %w", err)
	}
	return &c, nil
}

// GetCollectionByName fetches a collection by name.
func (d *DB) GetCollectionByName(ctx context.Context, name string) (*models.Collection, error) {
	slog.Debug("GetCollectionByName", "name", name)
	var c models.Collection
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, tagger_version, created_at FROM collections WHERE name = $1`,
		name,
	).Scan(&c.ID, &c.Name, &c.TaggerVersion, &c.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("collection not found: %w", err)
		}
		return nil, fmt.Errorf("get collection: %w", err)
	}
	return &c, nil
}

// CollectionCursor is a position in the order ListCollections returns collections in: the last collection
// of a page.
type CollectionCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListCollections returns up to limit collections, newest first (ties broken by id), that come after the
// cursor, or from the start when after is nil. The second result says whether more collections follow.
func (d *DB) ListCollections(ctx context.Context, limit int, after *CollectionCursor) ([]models.Collection, bool, error) {
	slog.Debug("ListCollections", "limit", limit, "paged", after != nil)
	const cols = `SELECT id, name, tagger_version, created_at FROM collections`
	const order = ` ORDER BY created_at DESC, id DESC LIMIT `
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = d.pool.Query(ctx, cols+order+`$1`, limit+1)
	} else {
		rows, err = d.pool.Query(ctx, cols+` WHERE (created_at, id) < ($1, $2::uuid)`+order+`$3`, after.CreatedAt, after.ID, limit+1)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	var collections []models.Collection
	for rows.Next() {
		var c models.Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.TaggerVersion, &c.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("scan collection: %w", err)
		}
		collections = append(collections, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list collections: %w", err)
	}
	more := len(collections) > limit
	if more {
		collections = collections[:limit]
	}
	return collections, more, nil
}

// GetCollectionPayloadKeys returns payload keys for all objects in a collection.
func (d *DB) GetCollectionPayloadKeys(ctx context.Context, collectionID string) ([]string, error) {
	slog.Debug("GetCollectionPayloadKeys", "collectionID", collectionID)
	rows, err := d.pool.Query(ctx,
		`SELECT payload_key FROM objects WHERE collection_id = $1`,
		collectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("get collection payload keys: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan payload key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// DeleteCollection deletes a collection by ID. Cascades to objects and tags via FK.
func (d *DB) DeleteCollection(ctx context.Context, collectionID string) error {
	slog.Debug("DeleteCollection", "collectionID", collectionID)
	_, err := d.pool.Exec(ctx,
		`DELETE FROM collections WHERE id = $1`,
		collectionID,
	)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
	}
	return nil
}

// GetObjectByID fetches object metadata by ID, joined with collection name.
func (d *DB) GetObjectByID(ctx context.Context, id string) (*models.Object, error) {
	slog.Debug("GetObjectByID", "id", id)
	var o models.Object
	var expiresAt *time.Time
	err := d.pool.QueryRow(ctx,
		`SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		 FROM objects o JOIN collections c ON o.collection_id = c.id
		 WHERE o.id = $1 AND (o.expires_at IS NULL OR o.expires_at > NOW())`,
		id,
	).Scan(&o.ID, &o.CollectionID, &o.Collection, &o.Metadata, &o.Date, &o.SizeBytes, &o.ContentHash, &o.CreatedAt, &expiresAt, &o.PayloadKey)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("object not found: %w", err)
		}
		return nil, fmt.Errorf("get object: %w", err)
	}
	o.ExpiresAt = expiresAt
	return &o, nil
}

// GetObjectByCollectionAndHash fetches an object by collection ID and content hash.
func (d *DB) GetObjectByCollectionAndHash(ctx context.Context, collectionID, hash string) (*models.Object, error) {
	slog.Debug("GetObjectByCollectionAndHash", "collectionID", collectionID, "hash", hash)
	var o models.Object
	var expiresAt *time.Time
	var collName string
	err := d.pool.QueryRow(ctx,
		`SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		 FROM objects o JOIN collections c ON o.collection_id = c.id
		 WHERE o.collection_id = $1 AND o.content_hash = $2 AND (o.expires_at IS NULL OR o.expires_at > NOW())`,
		collectionID, hash,
	).Scan(&o.ID, &o.CollectionID, &collName, &o.Metadata, &o.Date, &o.SizeBytes, &o.ContentHash, &o.CreatedAt, &expiresAt, &o.PayloadKey)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("object not found: %w", err)
		}
		return nil, fmt.Errorf("get object by hash: %w", err)
	}
	o.Collection = collName
	o.ExpiresAt = expiresAt
	return &o, nil
}

// GetObjectByCollectionAndHashIncludingExpired fetches an object by collection ID and content hash, including expired rows.
func (d *DB) GetObjectByCollectionAndHashIncludingExpired(ctx context.Context, collectionID, hash string) (*models.Object, error) {
	slog.Debug("GetObjectByCollectionAndHashIncludingExpired", "collectionID", collectionID, "hash", hash)
	var o models.Object
	var expiresAt *time.Time
	var collName string
	err := d.pool.QueryRow(ctx,
		`SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		 FROM objects o JOIN collections c ON o.collection_id = c.id
		 WHERE o.collection_id = $1 AND o.content_hash = $2`,
		collectionID, hash,
	).Scan(&o.ID, &o.CollectionID, &collName, &o.Metadata, &o.Date, &o.SizeBytes, &o.ContentHash, &o.CreatedAt, &expiresAt, &o.PayloadKey)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("object not found: %w", err)
		}
		return nil, fmt.Errorf("get object by hash: %w", err)
	}
	o.Collection = collName
	o.ExpiresAt = expiresAt
	return &o, nil
}

// InsertObject inserts a new object.
func (d *DB) InsertObject(ctx context.Context, collectionID string, hash string, date time.Time, sizeBytes int64, payloadKey string, metadata map[string]string, expiresAt *time.Time) (*models.Object, error) {
	slog.Debug("InsertObject", "collectionID", collectionID, "sizeBytes", sizeBytes, "metadataKeys", len(metadata))
	if metadata == nil {
		metadata = map[string]string{}
	}
	var id string
	var createdAt time.Time
	err := d.pool.QueryRow(ctx,
		`INSERT INTO objects (collection_id, content_hash, date, size_bytes, metadata, expires_at, payload_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`,
		collectionID, hash, date, sizeBytes, metadata, expiresAt, payloadKey,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert object: %w", err)
	}
	return &models.Object{
		ID:          id,
		Collection:  "", // filled by caller if needed
		Date:        date,
		SizeBytes:   sizeBytes,
		ContentHash: hash,
		CreatedAt:   createdAt,
		ExpiresAt:   expiresAt,
		PayloadKey:  payloadKey,
		Metadata:    metadata,
	}, nil
}

// ErrObjectNotFound is returned by UpdateObjectMetadata for an object that does not exist or has expired.
var ErrObjectNotFound = errors.New("object not found")

// UpdateObjectMetadata changes the metadata of an object in one transaction: change receives the current
// metadata and returns the new one, which replaces it. An error from change is returned as it is, and
// nothing is written. It returns the metadata that was stored.
func (d *DB) UpdateObjectMetadata(ctx context.Context, id string, change func(current map[string]string) (map[string]string, error)) (map[string]string, error) {
	slog.Debug("UpdateObjectMetadata", "id", id)
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit

	var current map[string]string
	err = tx.QueryRow(ctx,
		`SELECT metadata FROM objects WHERE id = $1 AND (expires_at IS NULL OR expires_at > NOW()) FOR UPDATE`, id,
	).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	updated, err := change(current)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		updated = map[string]string{}
	}
	if _, err := tx.Exec(ctx, `UPDATE objects SET metadata = $2 WHERE id = $1`, id, updated); err != nil {
		return nil, fmt.Errorf("write metadata: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return updated, nil
}

// DeleteObject removes object, tags, and returns payload_key.
func (d *DB) DeleteObject(ctx context.Context, id string) (string, error) {
	slog.Debug("DeleteObject", "id", id)
	var payloadKey string
	err := retryOnDeadlock(ctx, func() error {
		return d.pool.QueryRow(ctx,
			`DELETE FROM objects WHERE id = $1 RETURNING payload_key`,
			id,
		).Scan(&payloadKey)
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("object not found: %w", err)
		}
		return "", fmt.Errorf("delete object: %w", err)
	}
	return payloadKey, nil
}

// GetTagsForObject returns known tags for an object.
func (d *DB) GetTagsForObject(ctx context.Context, objectID string) (map[string]bool, error) {
	slog.Debug("GetTagsForObject", "objectID", objectID)
	rows, err := d.pool.Query(ctx,
		`SELECT tag, value FROM object_tags WHERE object_id = $1`,
		objectID,
	)
	if err != nil {
		return nil, fmt.Errorf("get tags: %w", err)
	}
	defer rows.Close()

	tags := make(map[string]bool)
	for rows.Next() {
		var tag string
		var value bool
		if err := rows.Scan(&tag, &value); err != nil {
			return nil, fmt.Errorf("scan tag: %w", err)
		}
		tags[tag] = value
	}
	return tags, rows.Err()
}

// UpsertTags inserts or updates tags for an object.
//
// The collection_tags counters are row-locked by triggers, so every writer
// follows one lock order to stay deadlock-free: the object row first (which is
// also what DeleteObject's cascade takes), then the counter rows in sorted tag
// order, then the collection row. Deadlocks that still occur (two writers
// racing to create the same brand-new counter) are retried.
func (d *DB) UpsertTags(ctx context.Context, collectionID, objectID string, tags map[string]bool) error {
	slog.Debug("UpsertTags", "collectionID", collectionID, "objectID", objectID, "tagCount", len(tags))
	if len(tags) == 0 {
		return nil
	}
	names := make([]string, 0, len(tags))
	for tag := range tags {
		names = append(names, tag)
	}
	sort.Strings(names)
	values := make([]bool, len(names))
	for i, tag := range names {
		values[i] = tags[tag]
	}

	return retryOnDeadlock(ctx, func() error {
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM objects WHERE id = $1 FOR KEY SHARE`, objectID).Scan(&one); err != nil {
			if err == pgx.ErrNoRows {
				return fmt.Errorf("upsert tags: object not found: %w", err)
			}
			return fmt.Errorf("lock object: %w", err)
		}
		// ON CONFLICT DO UPDATE fires the insert trigger and then the update
		// trigger, each locking only its own subset of counters. Locking every
		// existing counter this call can touch up front, in sorted order, keeps
		// two writers from taking those subsets in opposite orders.
		if _, err := tx.Exec(ctx,
			`SELECT 1 FROM collection_tags
			 WHERE collection_id = $1 AND tag = ANY($2::text[])
			 ORDER BY tag FOR UPDATE`,
			collectionID, names,
		); err != nil {
			return fmt.Errorf("lock tag counters: %w", err)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO object_tags (object_id, collection_id, tag, value, updated_at)
			 SELECT $1, $2, t.tag, t.value, NOW()
			 FROM unnest($3::text[], $4::boolean[]) AS t(tag, value)
			 ORDER BY t.tag
			 ON CONFLICT (object_id, tag) DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
			objectID, collectionID, names, values,
		)
		if err != nil {
			return fmt.Errorf("upsert tags: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit tx: %w", err)
		}
		return nil
	})
}

// retryOnDeadlock runs fn, retrying a few times with a short randomized backoff
// when Postgres aborts it as a deadlock victim.
func retryOnDeadlock(ctx context.Context, fn func() error) error {
	const attempts = 5
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = fn(); err == nil || !isDeadlock(err) {
			return err
		}
		slog.Warn("deadlock detected, retrying", "attempt", attempt+1)
		backoff := time.Duration(5+rand.Intn(20)*(attempt+1)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return err
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// GetCollectionTagStats returns the number of objects in a collection and the
// tags registered in it, ordered by tag name (byte order). prefix filters tags by prefix,
// afterTag is an exclusive keyset cursor ("" for the first page), and at most
// limit tags are returned.
//
// Counters are maintained by triggers (migration 000003) and are corrected here
// for objects that have expired but are not yet swept by the retention job, so
// the numbers match what object reads and queries return.
func (d *DB) GetCollectionTagStats(ctx context.Context, collectionID, prefix, afterTag string, limit int) (int64, []models.TagStat, error) {
	slog.Debug("GetCollectionTagStats", "collectionID", collectionID, "prefix", prefix, "afterTag", afterTag, "limit", limit)
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var total int64
	err = tx.QueryRow(ctx,
		`SELECT c.object_count - (
		     SELECT count(*) FROM objects o
		     WHERE o.collection_id = c.id AND o.expires_at IS NOT NULL AND o.expires_at <= NOW()
		 )
		 FROM collections c WHERE c.id = $1`,
		collectionID,
	).Scan(&total)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, nil, fmt.Errorf("collection not found: %w", err)
		}
		return 0, nil, fmt.Errorf("get collection object count: %w", err)
	}

	rows, err := tx.Query(ctx,
		`WITH expired AS (
		     SELECT t.tag,
		            count(*) FILTER (WHERE t.value) AS true_count,
		            count(*) FILTER (WHERE NOT t.value) AS false_count
		     FROM objects o JOIN object_tags t ON t.object_id = o.id
		     WHERE o.collection_id = $1 AND o.expires_at IS NOT NULL AND o.expires_at <= NOW()
		     GROUP BY t.tag
		 )
		 SELECT ct.tag,
		        ct.true_count - COALESCE(e.true_count, 0),
		        ct.false_count - COALESCE(e.false_count, 0),
		        ct.first_seen_at
		 FROM collection_tags ct LEFT JOIN expired e ON e.tag = ct.tag
		 WHERE ct.collection_id = $1 AND starts_with(ct.tag, $2) AND ct.tag COLLATE "C" > $3
		 ORDER BY ct.tag COLLATE "C"
		 LIMIT $4`,
		collectionID, prefix, afterTag, limit,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("get collection tag stats: %w", err)
	}
	defer rows.Close()

	var stats []models.TagStat
	for rows.Next() {
		var st models.TagStat
		if err := rows.Scan(&st.Tag, &st.TrueCount, &st.FalseCount, &st.FirstSeenAt); err != nil {
			return 0, nil, fmt.Errorf("scan tag stat: %w", err)
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("iterate tag stats: %w", err)
	}
	return total, stats, nil
}

// ExpiredObject identifies an object whose expiry has passed, with what the
// retention sweeper needs to remove it: its payload key and its position in
// the (expires_at, id) ordering.
type ExpiredObject struct {
	ID         string
	PayloadKey string
	ExpiresAt  time.Time
}

// ListExpiredObjects returns up to limit expired objects ordered by
// (expires_at, id), oldest first. When after is non-nil, only objects strictly
// after it in that ordering are returned, so a caller can walk the whole
// backlog page by page and move past rows it failed to remove.
func (d *DB) ListExpiredObjects(ctx context.Context, after *ExpiredObject, limit int) ([]ExpiredObject, error) {
	slog.Debug("ListExpiredObjects", "limit", limit, "paged", after != nil)
	var afterAt, afterID any // NULL on the first page
	if after != nil {
		afterAt, afterID = after.ExpiresAt, after.ID
	}
	rows, err := d.pool.Query(ctx,
		`SELECT id, payload_key, expires_at FROM objects
		 WHERE expires_at IS NOT NULL AND expires_at <= NOW()
		   AND ($2::timestamptz IS NULL OR (expires_at, id) > ($2::timestamptz, $3::uuid))
		 ORDER BY expires_at, id
		 LIMIT $1`,
		limit, afterAt, afterID,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	defer rows.Close()

	var out []ExpiredObject
	for rows.Next() {
		var o ExpiredObject
		if err := rows.Scan(&o.ID, &o.PayloadKey, &o.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan expired object: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// QueryObjectsKnownTags returns objects that already have all tags known and matching.
func (d *DB) QueryObjectsKnownTags(ctx context.Context, collectionID string, tags map[string]bool, dateFilter *models.DateFilter, cursorDate time.Time, cursorID string, limit int) ([]models.Object, error) {
	slog.Debug("QueryObjectsKnownTags", "collectionID", collectionID, "tagCount", len(tags), "limit", limit)
	// Build a query that finds objects where for every tag in the request,
	// there exists a row in object_tags with that value.
	// This is done via aggregation to avoid joins that could produce wrong counts.
	if len(tags) == 0 {
		return d.queryObjectsByDate(ctx, collectionID, dateFilter, cursorDate, cursorID, limit)
	}

	args := []any{collectionID}
	argIdx := 1

	var conds []string

	if cursorID != "" {
		argIdx++
		conds = append(conds, fmt.Sprintf("(o.date < $%d OR (o.date = $%d AND o.id > $%d))", argIdx, argIdx, argIdx+1))
		args = append(args, cursorDate, cursorID)
		argIdx = len(args) // last placeholder in use
	}

	// date filter
	dateConds, dateArgs, di := buildDateConds("o.date", argIdx, dateFilter)
	if len(dateConds) > 0 {
		conds = append(conds, dateConds...)
		args = append(args, dateArgs...)
		argIdx = di
	}

	// We need objects where all requested tags are known and match.
	// Strategy: use a lateral subquery or CTE. Let's use a CTE.
	// Actually, simpler: select from objects where id IN (
	//   SELECT object_id FROM object_tags WHERE collection_id=$1 AND (tag=$2 AND value=$3 OR ...)
	//   GROUP BY object_id HAVING COUNT(*) = N
	// ) and then order by date desc, id asc.

	tagConds := []string{}
	tagIdx := 0
	for tag, value := range tags {
		argIdx++
		cond := fmt.Sprintf("(tag = $%d AND value = $%d)", argIdx, argIdx+1)
		tagConds = append(tagConds, cond)
		args = append(args, tag, value)
		argIdx++
		tagIdx++
	}

	args = append(args, limit)
	limitArg := len(args)

	whereClause := ""
	if len(conds) > 0 {
		whereClause = "AND " + stringsJoin(" AND ", conds)
	}

	tagWhere := stringsJoin(" OR ", tagConds)

	query := fmt.Sprintf(`
		WITH matching_objects AS (
			SELECT object_id
			FROM object_tags
			WHERE collection_id = $1 AND (%s)
			GROUP BY object_id
			HAVING COUNT(*) = %d
		)
		SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		FROM objects o
		JOIN collections c ON o.collection_id = c.id
		WHERE o.collection_id = $1 %s
		  AND (o.expires_at IS NULL OR o.expires_at > NOW())
		  AND o.id IN (SELECT object_id FROM matching_objects)
		ORDER BY o.date DESC, o.id ASC
		LIMIT $%d
	`, tagWhere, len(tags), whereClause, limitArg)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query known tags: %w", err)
	}
	defer rows.Close()

	return scanObjects(rows)
}

func (d *DB) queryObjectsByDate(ctx context.Context, collectionID string, dateFilter *models.DateFilter, cursorDate time.Time, cursorID string, limit int) ([]models.Object, error) {
	slog.Debug("queryObjectsByDate", "collectionID", collectionID, "limit", limit)
	args := []any{collectionID}
	argIdx := 1

	var conds []string

	if cursorID != "" {
		argIdx++
		conds = append(conds, fmt.Sprintf("(o.date < $%d OR (o.date = $%d AND o.id > $%d))", argIdx, argIdx, argIdx+1))
		args = append(args, cursorDate, cursorID)
		argIdx = len(args) // last placeholder in use
	}

	dateConds, dateArgs, di := buildDateConds("o.date", argIdx, dateFilter)
	if len(dateConds) > 0 {
		conds = append(conds, dateConds...)
		args = append(args, dateArgs...)
		argIdx = di
	}

	args = append(args, limit)
	limitArg := len(args)

	whereClause := ""
	if len(conds) > 0 {
		whereClause = "AND " + stringsJoin(" AND ", conds)
	}

	query := fmt.Sprintf(`
		SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		FROM objects o
		JOIN collections c ON o.collection_id = c.id
		WHERE o.collection_id = $1 %s
		  AND (o.expires_at IS NULL OR o.expires_at > NOW())
		ORDER BY o.date DESC, o.id ASC
		LIMIT $%d
	`, whereClause, limitArg)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query by date: %w", err)
	}
	defer rows.Close()

	return scanObjects(rows)
}

// ScanCandidateObjects returns objects in order for further tag evaluation.
func (d *DB) ScanCandidateObjects(ctx context.Context, collectionID string, dateFilter *models.DateFilter, cursorDate time.Time, cursorID string, limit int) ([]models.Object, error) {
	slog.Debug("ScanCandidateObjects", "collectionID", collectionID, "limit", limit)
	args := []any{collectionID}
	argIdx := 1

	var conds []string

	if cursorID != "" {
		argIdx++
		conds = append(conds, fmt.Sprintf("(o.date < $%d OR (o.date = $%d AND o.id > $%d))", argIdx, argIdx, argIdx+1))
		args = append(args, cursorDate, cursorID)
		argIdx = len(args) // last placeholder in use
	}

	dateConds, dateArgs, di := buildDateConds("o.date", argIdx, dateFilter)
	if len(dateConds) > 0 {
		conds = append(conds, dateConds...)
		args = append(args, dateArgs...)
		argIdx = di
	}

	args = append(args, limit)
	limitArg := len(args)

	whereClause := ""
	if len(conds) > 0 {
		whereClause = "AND " + stringsJoin(" AND ", conds)
	}

	query := fmt.Sprintf(`
		SELECT o.id, o.collection_id, c.name, o.metadata, o.date, o.size_bytes, o.content_hash, o.created_at, o.expires_at, o.payload_key
		FROM objects o
		JOIN collections c ON o.collection_id = c.id
		WHERE o.collection_id = $1 %s
		  AND (o.expires_at IS NULL OR o.expires_at > NOW())
		ORDER BY o.date DESC, o.id ASC
		LIMIT $%d
	`, whereClause, limitArg)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("scan candidates: %w", err)
	}
	defer rows.Close()

	return scanObjects(rows)
}

// GetKnownTagsForObjects returns all known tags for a set of object IDs.
func (d *DB) GetKnownTagsForObjects(ctx context.Context, objectIDs []string) (map[string]map[string]bool, error) {
	slog.Debug("GetKnownTagsForObjects", "objectCount", len(objectIDs))
	if len(objectIDs) == 0 {
		return map[string]map[string]bool{}, nil
	}
	// pgx.In doesn't work with string slices directly in v5 the same way.
	// We'll construct IN clause manually.
	args := make([]any, len(objectIDs))
	placeholders := make([]string, len(objectIDs))
	for i, id := range objectIDs {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`SELECT object_id, tag, value FROM object_tags WHERE object_id IN (%s)`, joinStrings(placeholders, ","))
	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get known tags: %w", err)
	}
	defer rows.Close()

	result := make(map[string]map[string]bool)
	for rows.Next() {
		var oid, tag string
		var value bool
		if err := rows.Scan(&oid, &tag, &value); err != nil {
			return nil, fmt.Errorf("scan known tag: %w", err)
		}
		if result[oid] == nil {
			result[oid] = make(map[string]bool)
		}
		result[oid][tag] = value
	}
	return result, rows.Err()
}

func buildDateConds(col string, startIdx int, df *models.DateFilter) ([]string, []any, int) {
	slog.Debug("buildDateConds", "col", col, "startIdx", startIdx)
	if df == nil {
		return nil, nil, startIdx
	}
	var conds []string
	var args []any
	idx := startIdx
	if df.GT != nil {
		idx++
		conds = append(conds, fmt.Sprintf("%s > $%d", col, idx))
		args = append(args, *df.GT)
	}
	if df.GTE != nil {
		idx++
		conds = append(conds, fmt.Sprintf("%s >= $%d", col, idx))
		args = append(args, *df.GTE)
	}
	if df.LT != nil {
		idx++
		conds = append(conds, fmt.Sprintf("%s < $%d", col, idx))
		args = append(args, *df.LT)
	}
	if df.LTE != nil {
		idx++
		conds = append(conds, fmt.Sprintf("%s <= $%d", col, idx))
		args = append(args, *df.LTE)
	}
	if df.EQ != nil {
		idx++
		conds = append(conds, fmt.Sprintf("%s = $%d", col, idx))
		args = append(args, *df.EQ)
	}
	return conds, args, idx
}

func stringsJoin(sep string, items []string) string {
	slog.Debug("stringsJoin", "sep", sep, "items", len(items))
	if len(items) == 0 {
		return ""
	}
	result := items[0]
	for i := 1; i < len(items); i++ {
		result += sep + items[i]
	}
	return result
}

func joinStrings(items []string, sep string) string {
	slog.Debug("joinStrings", "items", len(items), "sep", sep)
	return stringsJoin(sep, items)
}

func scanObjects(rows pgx.Rows) ([]models.Object, error) {
	slog.Debug("scanObjects: starting scan")
	defer rows.Close()
	var objs []models.Object
	for rows.Next() {
		var o models.Object
		var expiresAt *time.Time
		if err := rows.Scan(&o.ID, &o.CollectionID, &o.Collection, &o.Metadata, &o.Date, &o.SizeBytes, &o.ContentHash, &o.CreatedAt, &expiresAt, &o.PayloadKey); err != nil {
			return nil, fmt.Errorf("scan object: %w", err)
		}
		o.ExpiresAt = expiresAt
		objs = append(objs, o)
	}
	return objs, rows.Err()
}
