package db_test

import (
	"context"
	"testing"
	"time"

	"mrsydar/tagona/storage/internal/db"
	"mrsydar/tagona/storage/internal/models"
)

// seedDated creates n objects, newest first: object i has date base - i hours.
func seedDated(t *testing.T, d *db.DB, collID string, n int) ([]string, time.Time) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		o, err := d.InsertObject(context.Background(), collID, string(rune('a'+i)), base.Add(-time.Duration(i)*time.Hour), 1, "key/"+string(rune('a'+i)), nil, nil)
		if err != nil {
			t.Fatalf("insert object: %v", err)
		}
		ids[i] = o.ID
	}
	return ids, base
}

func idsOf(objs []models.Object) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.ID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A cursor combined with a date filter (page 2+ of a date-filtered query) must
// keep SQL placeholders and arguments aligned.
func TestScanCandidateObjectsCursorWithDateFilter(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	ids, base := seedDated(t, d, coll, 6) // dates: base, -1h, ... -5h
	filter := &models.DateFilter{GTE: ptr(base.Add(-4 * time.Hour))}

	page1, err := d.ScanCandidateObjects(ctx, coll, filter, time.Time{}, "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !equalIDs(idsOf(page1), ids[0:2]) {
		t.Fatalf("page 1 = %v, want %v", idsOf(page1), ids[0:2])
	}
	last := page1[len(page1)-1]
	page2, err := d.ScanCandidateObjects(ctx, coll, filter, last.Date, last.ID, 10)
	if err != nil {
		t.Fatalf("page 2 with cursor and date filter: %v", err)
	}
	if !equalIDs(idsOf(page2), ids[2:5]) { // -2h, -3h, -4h; -5h is filtered out
		t.Errorf("page 2 = %v, want %v", idsOf(page2), ids[2:5])
	}
}

func TestQueryObjectsKnownTags(t *testing.T) {
	d, _ := newTestDB(t)
	ctx := context.Background()
	coll := mustCollection(t, d, "jobs")
	ids, base := seedDated(t, d, coll, 5)
	mustTag(t, d, coll, ids[0], map[string]bool{"a": true, "b": true})
	mustTag(t, d, coll, ids[1], map[string]bool{"a": true, "b": false})
	mustTag(t, d, coll, ids[2], map[string]bool{"a": true}) // b unknown
	mustTag(t, d, coll, ids[3], map[string]bool{"a": false, "b": true})
	// ids[4] has no tags at all

	query := func(tags map[string]bool, filter *models.DateFilter, cd time.Time, cid string, limit int) []string {
		t.Helper()
		objs, err := d.QueryObjectsKnownTags(ctx, coll, tags, filter, cd, cid, limit)
		if err != nil {
			t.Fatalf("query %v: %v", tags, err)
		}
		return idsOf(objs)
	}
	check := func(name string, got, want []string) {
		t.Helper()
		if !equalIDs(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	check("a=true,b=true", query(map[string]bool{"a": true, "b": true}, nil, time.Time{}, "", 10), []string{ids[0]})
	check("a=true", query(map[string]bool{"a": true}, nil, time.Time{}, "", 10), []string{ids[0], ids[1], ids[2]})
	check("b=false", query(map[string]bool{"b": false}, nil, time.Time{}, "", 10), []string{ids[1]})
	check("a=true,b=false", query(map[string]bool{"a": true, "b": false}, nil, time.Time{}, "", 10), []string{ids[1]})
	check("unknown tag excludes everything", query(map[string]bool{"zzz": true}, nil, time.Time{}, "", 10), []string{})
	check("no tags = all, by date", query(nil, nil, time.Time{}, "", 10), ids)

	// Paging: limit 2 then continue from the cursor.
	page1 := query(map[string]bool{"a": true}, nil, time.Time{}, "", 2)
	check("page 1", page1, []string{ids[0], ids[1]})
	objs, _ := d.QueryObjectsKnownTags(ctx, coll, map[string]bool{"a": true}, nil, time.Time{}, "", 2)
	last := objs[len(objs)-1]
	check("page 2", query(map[string]bool{"a": true}, nil, last.Date, last.ID, 2), []string{ids[2]})

	// No tags + cursor goes through the by-date query.
	check("no tags + cursor", query(nil, nil, last.Date, last.ID, 10), ids[2:])

	// Cursor and date filter together.
	filter := &models.DateFilter{GTE: ptr(base.Add(-2 * time.Hour))}
	check("cursor + date filter", query(map[string]bool{"a": true}, filter, last.Date, last.ID, 10), []string{ids[2]})
	check("date filter only", query(map[string]bool{"a": true}, filter, time.Time{}, "", 10), []string{ids[0], ids[1], ids[2]})
}

func ptr[T any](v T) *T { return &v }
