package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"mrsydar/tagona/keystorage/internal/db"
	"mrsydar/tagona/keystorage/internal/dbtest"
	"mrsydar/tagona/keystorage/internal/keys"
	"mrsydar/tagona/keystorage/migrations"
)

func TestCreateGetListDelete(t *testing.T) {
	d, _ := dbtest.New(t)
	ctx := context.Background()

	raw, hash, prefix, err := keys.Generate()
	if err != nil {
		t.Fatal(err)
	}
	created, err := d.Create(ctx, hash, prefix, "dev", 0)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Name != "dev" || created.KeyPrefix != prefix {
		t.Fatalf("unexpected key: %+v", created)
	}

	got, err := d.GetByHash(ctx, keys.Hash(raw))
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetByHash = %+v, %v", got, err)
	}
	if _, err := d.GetByHash(ctx, keys.Hash("tagona_unknown")); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("unknown key: err = %v, want ErrNotFound", err)
	}

	list, err := d.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("List = %+v, %v", list, err)
	}

	if ok, err := d.Delete(ctx, created.ID); err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	if ok, err := d.Delete(ctx, created.ID); err != nil || ok {
		t.Fatalf("second Delete = %v, %v; want false, nil", ok, err)
	}
	if _, err := d.GetByHash(ctx, keys.Hash(raw)); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("deleted key still validates: %v", err)
	}
}

func TestDuplicateHashRejected(t *testing.T) {
	d, _ := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, "h", "p", "a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, "h", "p", "b", 0); err == nil {
		t.Fatal("expected a unique violation")
	}
}

// The migrations run on every service start, so repeating them must be harmless and keep the data.
func TestMigrationsAreIdempotent(t *testing.T) {
	d, pool := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, "h", "p", "kept", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	list, err := d.List(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "kept" {
		t.Fatalf("data lost after re-running migrations: %+v, %v", list, err)
	}
}

func TestExpiry(t *testing.T) {
	d, pool := dbtest.New(t)
	ctx := context.Background()

	forever, err := d.Create(ctx, keys.Hash("forever"), "p", "forever", 0)
	if err != nil || forever.ExpiresAt != nil {
		t.Fatalf("a key without a ttl has no expiry: %+v, %v", forever, err)
	}
	short, err := d.Create(ctx, keys.Hash("short"), "p", "short", 3600)
	if err != nil || short.ExpiresAt == nil {
		t.Fatalf("create with ttl: %+v, %v", short, err)
	}
	if until := time.Until(*short.ExpiresAt); until < 59*time.Minute || until > 61*time.Minute {
		t.Fatalf("expires in %v, want about an hour", until)
	}

	if k, err := d.GetByHash(ctx, keys.Hash("short")); err != nil || k.ExpiresAt == nil {
		t.Fatalf("a live key validates: %+v, %v", k, err)
	}

	// Move the expiry into the past by the database's own clock.
	if _, err := pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE name = 'short'`); err != nil {
		t.Fatal(err)
	}
	k, err := d.GetByHash(ctx, keys.Hash("short"))
	if !errors.Is(err, db.ErrExpired) || k == nil || k.Name != "short" {
		t.Fatalf("an expired key: %+v, %v; want the key and ErrExpired", k, err)
	}
	if _, err := d.GetByHash(ctx, keys.Hash("forever")); err != nil {
		t.Fatalf("a key without expiry is unaffected: %v", err)
	}
	if list, _ := d.List(ctx); len(list) != 2 {
		t.Fatalf("an expired key stays listed until it is swept: %d keys", len(list))
	}
}

func TestDeleteExpiredHonoursRetention(t *testing.T) {
	d, pool := dbtest.New(t)
	ctx := context.Background()
	for _, name := range []string{"forever", "live", "just-expired", "long-expired"} {
		ttl := int64(0)
		if name != "forever" {
			ttl = 3600
		}
		if _, err := d.Create(ctx, keys.Hash(name), "p", name, ttl); err != nil {
			t.Fatal(err)
		}
	}
	pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE name = 'just-expired'`)
	pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '3 days' WHERE name = 'long-expired'`)

	n, err := d.DeleteExpired(ctx, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("with a day of retention only the 3-day-old key goes: %d, %v", n, err)
	}
	n, err = d.DeleteExpired(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("with no retention the hour-old one goes too: %d, %v", n, err)
	}
	list, _ := d.List(ctx)
	if len(list) != 2 {
		t.Fatalf("keys that never expire and live ones stay: %d left", len(list))
	}
}

// A database that has keys from before expiry existed: the migration adds the column and the keys
// keep working, as keys that never expire.
func TestMigrationKeepsKeysCreatedBeforeExpiryExisted(t *testing.T) {
	d, pool := dbtest.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE api_keys DROP COLUMN expires_at`); err != nil { // the schema as it was after 000001
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO api_keys (key_hash, key_prefix, name) VALUES ($1, 'tagona_old12', 'legacy')`, keys.Hash("tagona_legacy")); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrating a database with existing keys: %v", err)
	}
	k, err := d.GetByHash(ctx, keys.Hash("tagona_legacy"))
	if err != nil || k.Name != "legacy" || k.ExpiresAt != nil {
		t.Fatalf("the legacy key must still validate and never expire: %+v, %v", k, err)
	}
}
