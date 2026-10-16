package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"yoursix-ifttt/internal/yoursix"
)

func TestParseSearch(t *testing.T) {
	got := parseSearch(` cam-01  site:"Head office" rule:gate "front door" groupName:west http://x weird: `)
	want := []searchTerm{
		{"", "cam-01"}, {"site", "Head office"}, {"rule", "gate"}, {"", "front door"},
		{"groupName", "west"}, {"http", "//x"}, {"", "weird:"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func openTest(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Events stored before the index existed are indexed on the next start,
// and search gives the same answers with and without the index.
func TestSearchIndexBackfillAndFallback(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	st := openTest(t, path)
	for _, code := range []string{"vmd", "aoa"} {
		ev := yoursix.Sample("device", code)
		raw, _ := json.Marshal(ev)
		if _, _, err := st.InsertEvent(ctx, ev, raw, "1.2.3.4", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`DROP TABLE events_fts`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st = openTest(t, path)
	if !st.fts {
		t.Fatal("index not available")
	}
	search := func(q string) int {
		evs, err := st.SearchEvents(ctx, EventQuery{Text: q})
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	for _, fts := range []bool{true, false} {
		st.fts = fts
		for q, want := range map[string]int{"device:aoa": 1, "DEVICE:VMD": 1, "camera 01": 2, "1.2.3": 2, "nothing-here": 0} {
			if got := search(q); got != want {
				t.Errorf("fts=%v %q: %d, want %d", fts, q, got, want)
			}
		}
	}
}
