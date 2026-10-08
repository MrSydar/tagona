package db_test

import (
	"context"
	"testing"
	"time"
)

func TestListExpiredObjects(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	now := time.Now().UTC()
	insert := func(hash string, expires *time.Time) string {
		o, err := d.InsertObject(ctx, coll, hash, now, 1, "jobs/"+hash, nil, expires)
		if err != nil {
			t.Fatalf("insert %s: %v", hash, err)
		}
		return o.ID
	}
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	a := insert("a", at(-3*time.Hour))
	b := insert("b", at(-2*time.Hour))
	c := insert("c", at(-1*time.Hour))
	insert("future", at(time.Hour)) // not expired yet
	insert("forever", nil)          // permanent

	all, err := d.ListExpiredObjects(ctx, nil, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 || all[0].ID != a || all[1].ID != b || all[2].ID != c {
		t.Fatalf("expired = %+v, want [a b c] oldest first and no live objects", all)
	}
	if all[0].PayloadKey != "jobs/a" || all[2].PayloadKey != "jobs/c" {
		t.Errorf("payload keys = %q .. %q, want the keys the sweeper needs to delete the payloads", all[0].PayloadKey, all[2].PayloadKey)
	}

	// Page by page with the keyset cursor.
	p1, _ := d.ListExpiredObjects(ctx, nil, 2)
	if len(p1) != 2 || p1[0].ID != a || p1[1].ID != b {
		t.Fatalf("page 1 = %+v", p1)
	}
	p2, _ := d.ListExpiredObjects(ctx, &p1[1], 2)
	if len(p2) != 1 || p2[0].ID != c {
		t.Fatalf("page 2 = %+v, want only c", p2)
	}
	p3, _ := d.ListExpiredObjects(ctx, &p2[0], 2)
	if len(p3) != 0 {
		t.Fatalf("page 3 = %+v, want the end of the backlog", p3)
	}

	// Removing a listed object takes it out of later listings.
	if _, err := d.DeleteObject(ctx, a); err != nil {
		t.Fatal(err)
	}
	rest, _ := d.ListExpiredObjects(ctx, nil, 100)
	if len(rest) != 2 || rest[0].ID != b {
		t.Errorf("after deleting a: %+v", rest)
	}
}

// Objects that expire at exactly the same instant (a bulk upload with one
// ttl_seconds) are ordered by id, and a cursor between them must neither skip
// nor repeat any.
func TestListExpiredObjectsPagesThroughTies(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	same := time.Now().UTC().Add(-time.Hour)
	want := map[string]bool{}
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		o, err := d.InsertObject(ctx, coll, h, same, 1, "jobs/"+h, nil, &same)
		if err != nil {
			t.Fatal(err)
		}
		want[o.ID] = true
	}

	seen := map[string]bool{}
	page, _ := d.ListExpiredObjects(ctx, nil, 2)
	for len(page) > 0 {
		for _, o := range page {
			if seen[o.ID] {
				t.Fatalf("object %s returned twice", o.ID)
			}
			seen[o.ID] = true
		}
		if len(page) < 2 {
			break
		}
		page, _ = d.ListExpiredObjects(ctx, &page[len(page)-1], 2)
	}
	if len(seen) != len(want) {
		t.Errorf("paged through %d of %d objects with identical expiry", len(seen), len(want))
	}
}
