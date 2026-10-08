package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mrsydar/tagona/storage/internal/db"
)

func TestCollectionsKeepTheirTaggerVersion(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	c, err := d.CreateCollection(ctx, "jobs", "decisions/openai:zai-org/GLM-5.3-Flash")
	if err != nil || c.TaggerVersion != "decisions/openai:zai-org/GLM-5.3-Flash" {
		t.Fatalf("%+v, %v", c, err)
	}
	got, err := d.GetCollectionByName(ctx, "jobs")
	if err != nil || got.TaggerVersion != c.TaggerVersion || got.ID != c.ID {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestListCollectionsPagesInOrder(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	var created []string
	for i := 0; i < 5; i++ {
		c, err := d.CreateCollection(ctx, fmt.Sprintf("c%d", i), "grep")
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, c.ID)
	}

	var got []string
	var after *db.CollectionCursor
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging does not end")
		}
		page, more, err := d.ListCollections(ctx, 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			got = append(got, c.ID)
		}
		if !more {
			break
		}
		last := page[len(page)-1]
		after = &db.CollectionCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	if len(got) != 5 {
		t.Fatalf("got %d collections: %v", len(got), got)
	}
	for i, id := range got { // newest first
		if id != created[len(created)-1-i] {
			t.Fatalf("order %v, want %v reversed", got, created)
		}
	}
	if all, more, err := d.ListCollections(ctx, 5, nil); err != nil || more || len(all) != 5 {
		t.Fatalf("an exact fit has no next page: %d, %v, %v", len(all), more, err)
	}
}

func TestObjectMetadata(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")

	o, err := d.InsertObject(ctx, coll, "h1", time.Now(), 1, "key/h1", map[string]string{"name": "cute-dog.png", "empty": ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetObjectByID(ctx, o.ID)
	if err != nil || len(got.Metadata) != 2 || got.Metadata["name"] != "cute-dog.png" || got.Metadata["empty"] != "" {
		t.Fatalf("%+v, %v", got, err)
	}

	// no metadata is an empty object, not null
	plain, err := d.InsertObject(ctx, coll, "h2", time.Now(), 1, "key/h2", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetObjectByID(ctx, plain.ID); got.Metadata == nil || len(got.Metadata) != 0 {
		t.Fatalf("metadata = %#v", got.Metadata)
	}

	// the change sees the current value and its result replaces it
	updated, err := d.UpdateObjectMetadata(ctx, o.ID, func(cur map[string]string) (map[string]string, error) {
		if cur["name"] != "cute-dog.png" {
			t.Errorf("current = %v", cur)
		}
		return map[string]string{"name": "dog.png", "kind": "photo"}, nil
	})
	if err != nil || updated["name"] != "dog.png" {
		t.Fatalf("%v, %v", updated, err)
	}
	if got, _ := d.GetObjectByID(ctx, o.ID); len(got.Metadata) != 2 || got.Metadata["kind"] != "photo" {
		t.Fatalf("%+v", got.Metadata)
	}

	// an error from the change writes nothing
	boom := fmt.Errorf("boom")
	if _, err := d.UpdateObjectMetadata(ctx, o.ID, func(map[string]string) (map[string]string, error) { return nil, boom }); err != boom {
		t.Fatalf("err = %v", err)
	}
	if got, _ := d.GetObjectByID(ctx, o.ID); got.Metadata["name"] != "dog.png" {
		t.Fatalf("a failed change was written: %v", got.Metadata)
	}

	// unknown and expired objects
	if _, err := d.UpdateObjectMetadata(ctx, "00000000-0000-4000-8000-000000000000", func(m map[string]string) (map[string]string, error) { return m, nil }); err != db.ErrObjectNotFound {
		t.Fatalf("unknown object: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	gone, _ := d.InsertObject(ctx, coll, "h3", time.Now(), 1, "key/h3", nil, &past)
	if _, err := d.UpdateObjectMetadata(ctx, gone.ID, func(m map[string]string) (map[string]string, error) { return m, nil }); err != db.ErrObjectNotFound {
		t.Fatalf("expired object: %v", err)
	}
}
