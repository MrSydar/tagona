// Package db is keystorage's access to the api_keys table.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound is returned when no key matches.
	ErrNotFound = errors.New("api key not found")
	// ErrExpired is returned together with the key when it exists but its expiry has passed.
	ErrExpired = errors.New("api key expired")
)

// APIKey describes a stored key. The raw key is never stored, only its hash.
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt is when the key stops working; nil for a key that never expires.
	ExpiresAt *time.Time `json:"expires_at"`
}

// DB wraps a pool whose search_path is the keys schema.
type DB struct {
	pool *pgxpool.Pool
}

// New wraps pool.
func New(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// Connect opens a pool for dsn that resolves unqualified names in schema only. The search_path is
// deliberately not extended with public: this service has no business there.
func Connect(ctx context.Context, dsn, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return pool, nil
}

// Migrate executes every *.up.sql file of fsys in lexicographic order. The files are idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("exec migration %s: %w", name, err)
		}
	}
	return nil
}

// Ping checks the database connection.
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Create inserts a key by its hash. A ttlSeconds above zero makes it expire that many seconds from
// now, by the database's clock, which is also the one that judges expiry.
func (d *DB) Create(ctx context.Context, keyHash, keyPrefix, name string, ttlSeconds int64) (*APIKey, error) {
	var k APIKey
	err := d.pool.QueryRow(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, name, expires_at)
		 VALUES ($1, $2, $3, CASE WHEN $4::bigint > 0 THEN now() + $4::bigint * interval '1 second' END)
		 RETURNING id, name, key_prefix, created_at, expires_at`,
		keyHash, keyPrefix, name, ttlSeconds,
	).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("insert api key: %w", err)
	}
	return &k, nil
}

// Cursor is a position in the order List returns keys in: the last key of a page.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// List returns up to limit keys, newest first (ties broken by id), that come after the cursor, or from
// the start when after is nil. The second result says whether more keys follow. Expired keys are
// included until they are swept.
func (d *DB) List(ctx context.Context, limit int, after *Cursor) ([]APIKey, bool, error) {
	const cols = `SELECT id, name, key_prefix, created_at, expires_at FROM api_keys`
	const order = ` ORDER BY created_at DESC, id DESC LIMIT `
	var rows pgx.Rows
	var err error
	if after == nil {
		rows, err = d.pool.Query(ctx, cols+order+`$1`, limit+1)
	} else {
		rows, err = d.pool.Query(ctx, cols+` WHERE (created_at, id) < ($1, $2::uuid)`+order+`$3`, after.CreatedAt, after.ID, limit+1)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, false, fmt.Errorf("scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list api keys: %w", err)
	}
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	return keys, more, nil
}

// GetByHash returns the key with the given hash. It returns ErrNotFound when there is none, and
// the key together with ErrExpired when its expiry has passed.
func (d *DB) GetByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	var k APIKey
	var expired bool
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, key_prefix, created_at, expires_at, (expires_at IS NOT NULL AND expires_at <= now())
		 FROM api_keys WHERE key_hash = $1`, keyHash,
	).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt, &k.ExpiresAt, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}
	if expired {
		return &k, ErrExpired
	}
	return &k, nil
}

// DeleteExpired removes the keys that expired more than retention ago and returns how many.
func (d *DB) DeleteExpired(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := d.pool.Exec(ctx,
		`DELETE FROM api_keys WHERE expires_at IS NOT NULL AND expires_at < now() - $1::bigint * interval '1 second'`,
		int64(retention.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("delete expired api keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Delete removes a key and reports whether one existed. id must be a UUID.
func (d *DB) Delete(ctx context.Context, id string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete api key: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
