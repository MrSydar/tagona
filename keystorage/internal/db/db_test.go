package db_test

import (
	"context"
	"errors"
	"testing"

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
	created, err := d.Create(ctx, hash, prefix, "dev")
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
	if _, err := d.Create(ctx, "h", "p", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(ctx, "h", "p", "b"); err == nil {
		t.Fatal("expected a unique violation")
	}
}

// The migrations run on every service start, so repeating them must be harmless and keep the data.
func TestMigrationsAreIdempotent(t *testing.T) {
	d, pool := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Create(ctx, "h", "p", "kept"); err != nil {
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
