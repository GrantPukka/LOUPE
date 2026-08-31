package store

import (
	"context"
	"testing"
)

// The cached field names must never be shorter than the truth.
//
// A missing name is not a slow answer but a wrong one: the filter language
// answers an unknown field with an error naming what exists, so a list that has
// gone stale refuses a field that is really there. The count the list was
// derived from is what makes it self-invalidating, and this is the test of that.
func TestStoredFieldNamesGoStaleWhenRecordsAreAppended(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	add(t, db, Source{Name: "app"},
		entry(1, "2026-08-13T14:00:00Z", "info", "a", map[string]any{"alpha": int64(1)}))

	first, err := db.Fields(ctx)
	if err != nil {
		t.Fatalf("fields: %v", err)
	}
	if err := db.StoreFieldNames(ctx, first); err != nil {
		t.Fatalf("store: %v", err)
	}

	got, ok, err := db.StoredFieldNames(ctx)
	if err != nil || !ok {
		t.Fatalf("stored names not usable straight after writing them: ok=%v err=%v", ok, err)
	}
	if !contains(got, "alpha") {
		t.Errorf("stored names lost a field: %v", got)
	}

	// A record arrives carrying a name the list has never seen.
	add(t, db, Source{Name: "app"},
		entry(2, "2026-08-13T14:00:01Z", "info", "b", map[string]any{"zulu": int64(2)}))

	if _, ok, err := db.StoredFieldNames(ctx); err != nil {
		t.Fatalf("stored names: %v", err)
	} else if ok {
		t.Error("the list was served after records were appended; it is missing zulu")
	}
}

// A corpus with no bag fields must record that fact, or every later command
// pays for the full scan again to rediscover that there is nothing there.
func TestStoredFieldNamesRecordsHavingNone(t *testing.T) {
	ctx := context.Background()
	db := open(t)

	add(t, db, Source{Name: "app"},
		entry(1, "2026-08-13T14:00:00Z", "info", "just a line of text", nil))

	if err := db.StoreFieldNames(ctx, nil); err != nil {
		t.Fatalf("store: %v", err)
	}

	got, ok, err := db.StoredFieldNames(ctx)
	if err != nil || !ok {
		t.Fatalf("having no fields was not recorded: ok=%v err=%v", ok, err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want no names at all", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
