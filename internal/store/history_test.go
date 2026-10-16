package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"yoursix-ifttt/internal/rules"
)

// An item that existed before history was kept gets its earlier state
// logged the first time it changes, so the change can be compared.
func TestHistoryBaseline(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := rules.Rule{Name: "Old", Enabled: true, Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 5}}}
	created, err := st.CreateRule(ctx, r, Editor{"a", "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM revisions`); err != nil {
		t.Fatal(err)
	}
	r.ID, r.Name = created.ID, "New"
	if _, err := st.UpdateRule(ctx, r, Editor{"b", "Bob"}); err != nil {
		t.Fatal(err)
	}
	revs, err := st.ListRevisions(ctx, HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[1].Change != ChangeExisting || revs[1].Name != "Old" || revs[1].ByName != "Alice" ||
		revs[0].Change != ChangeUpdated || revs[0].Name != "New" || revs[0].ByName != "Bob" {
		t.Fatalf("history: %+v", revs)
	}
}

// Credentials typed into an action are hidden in history; references to
// secrets are kept.
func TestHistoryHidesLiteralCredentials(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	http := func(pw, auth string) rules.Action {
		return rules.Action{Type: rules.ActionHTTP, URL: "http://cam/x", Auth: "basic", Username: "u", Password: pw,
			Headers: map[string]string{"Authorization": auth, "X-Trace": "keep-me"}}
	}
	if _, err := st.CreateRequest(ctx, rules.SavedRequest{Name: "cam", Request: http("hunter2", "Bearer abc")}, Editor{"a", "Alice"}); err != nil {
		t.Fatal(err)
	}
	r := rules.Rule{Name: "r", Enabled: true, Actions: []rules.Action{http(`{{ secret "CAM" }}`, "Bearer abc")},
		OnFailure: []rules.Action{http("hunter2", "")}}
	if _, err := st.CreateRule(ctx, r, Editor{"a", "Alice"}); err != nil {
		t.Fatal(err)
	}
	revs, err := st.ListRevisions(ctx, HistoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rev := range revs {
		s := string(rev.Snapshot)
		if strings.Contains(s, "hunter2") || strings.Contains(s, "Bearer abc") {
			t.Errorf("%s snapshot leaks a credential: %s", rev.Kind, s)
		}
		if !strings.Contains(s, "keep-me") {
			t.Errorf("%s snapshot lost an ordinary header: %s", rev.Kind, s)
		}
		if rev.Kind == KindRule && !strings.Contains(s, `{{ secret \"CAM\" }}`) {
			t.Errorf("secret reference was hidden: %s", s)
		}
	}
}

// A save and its history entry happen together: if the history write
// fails, the change is rolled back rather than saved without a record.
func TestSaveRollsBackWithoutHistory(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := rules.Rule{Name: "Keep", Enabled: true, Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 5}}}
	created, err := st.CreateRule(ctx, r, Editor{"a", "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := st.CreateSchedule(ctx, rules.Schedule{Name: "Nights", Windows: []rules.Window{{From: "22:00", To: "06:00"}}}, Editor{"a", "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE revisions`); err != nil {
		t.Fatal(err)
	}

	r.ID, r.Name = created.ID, "Changed"
	if _, err := st.UpdateRule(ctx, r, Editor{"b", "Bob"}); err == nil {
		t.Fatal("update should fail without a history table")
	}
	if got, _ := st.GetRule(ctx, created.ID); got.Name != "Keep" {
		t.Errorf("rule changed anyway: %q", got.Name)
	}
	if err := st.DeleteSchedule(ctx, sc.ID, Editor{"b", "Bob"}); err == nil {
		t.Fatal("delete should fail without a history table")
	}
	if _, err := st.Schedule(ctx, sc.ID); err != nil {
		t.Errorf("schedule deleted anyway: %v", err)
	}
	if _, err := st.CreateRequest(ctx, rules.SavedRequest{Name: "x", Request: rules.Action{Type: rules.ActionDelay, DelayMillis: 1}}, Editor{}); err == nil {
		t.Fatal("create should fail without a history table")
	}
	if list, _ := st.ListRequests(ctx); len(list) != 0 {
		t.Errorf("saved action created anyway: %+v", list)
	}
}
