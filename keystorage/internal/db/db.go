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

// ErrNotFound is returned when no key matches.
var ErrNotFound = errors.New("api key not found")

// APIKey describes a stored key. The raw key is never stored, only its hash.
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
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

// Create inserts a key by its hash.
func (d *DB) Create(ctx context.Context, keyHash, keyPrefix, name string) (*APIKey, error) {
	var k APIKey
	err := d.pool.QueryRow(ctx,
		`INSERT INTO api_keys (key_hash, key_prefix, name) VALUES ($1, $2, $3) RETURNING id, name, key_prefix, created_at`,
		keyHash, keyPrefix, name,
	).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert api key: %w", err)
	}
	return &k, nil
}

// List returns all keys, newest first.
func (d *DB) List(ctx context.Context) ([]APIKey, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, name, key_prefix, created_at FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// GetByHash returns the key with the given hash, or ErrNotFound.
func (d *DB) GetByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	var k APIKey
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, key_prefix, created_at FROM api_keys WHERE key_hash = $1`, keyHash,
	).Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get api key: %w", err)
	}
	return &k, nil
}

// Delete removes a key and reports whether one existed. id must be a UUID.
func (d *DB) Delete(ctx context.Context, id string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete api key: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
