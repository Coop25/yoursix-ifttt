package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"yoursix-ifttt/internal/store"
)

func TestSchedules(t *testing.T) {
	ta := newTwoAdmins(t)
	sched := map[string]any{
		"name":    "Weeknights",
		"windows": []map[string]any{{"days": []string{"mon", "tue", "wed", "thu", "fri"}, "from": "22:00", "to": "06:00"}},
	}
	id := ta.create(t, ta.alice, "/api/schedules", sched)
	code, body := ta.do(t, ta.alice, "GET", fmt.Sprintf("/api/schedules/%d", id), nil)
	if code != http.StatusOK || !strings.Contains(body, `"timezone":"America/Chicago"`) {
		t.Fatalf("get: %d %s (time zone should default to Chicago)", code, body)
	}

	rule := map[string]any{
		"name": "Siren", "enabled": true, "scheduleId": id,
		"actions": []map[string]any{{"type": "delay", "delayMillis": 5}},
	}
	ruleID := ta.create(t, ta.alice, "/api/rules", rule)

	// A rule can't point at a missing schedule.
	rule["scheduleId"] = 9999
	if code, body := ta.do(t, ta.alice, "POST", "/api/rules", rule); code != 400 || !strings.Contains(body, "doesn't exist") {
		t.Fatalf("missing schedule: %d %s", code, body)
	}

	// Listed with the rules that use it; can't be deleted while used.
	_, body = ta.do(t, ta.alice, "GET", "/api/schedules", nil)
	var list []store.ScheduleView
	_ = json.Unmarshal([]byte(body), &list)
	if len(list) != 1 || len(list[0].UsedBy) != 1 || list[0].UsedBy[0].ID != ruleID {
		t.Fatalf("list: %s", body)
	}
	if code, _ := ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/schedules/%d", id), nil); code != http.StatusConflict {
		t.Fatalf("delete in use: %d", code)
	}

	// Validation, conflicts and history like the other editors.
	if code, _ := ta.do(t, ta.alice, "POST", "/api/schedules", map[string]any{"name": "x", "windows": []any{}}); code != 400 {
		t.Errorf("no windows: %d", code)
	}
	if code, _ := ta.do(t, ta.alice, "POST", "/api/schedules", map[string]any{"name": "x", "timezone": "Nowhere/Land", "windows": []map[string]any{{}}}); code != 400 {
		t.Errorf("bad zone: %d", code)
	}
	sched["timezone"] = "Europe/London"
	sched["updatedAt"] = "2000-01-01T00:00:00Z"
	if code, _ := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/schedules/%d", id), sched); code != http.StatusConflict {
		t.Errorf("stale save: %d", code)
	}
	delete(sched, "updatedAt")
	if code, body := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/schedules/%d", id), sched); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	_, body = ta.do(t, ta.alice, "GET", "/api/history?kind=schedule", nil)
	var revs []store.Revision
	_ = json.Unmarshal([]byte(body), &revs)
	if len(revs) != 2 || revs[0].Change != "updated" || revs[0].ByID != "bob" {
		t.Fatalf("history: %s", body)
	}

	// Free once no rule uses it.
	rule["scheduleId"] = 0
	ta.do(t, ta.alice, "PUT", fmt.Sprintf("/api/rules/%d", ruleID), rule)
	if code, _ := ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/schedules/%d", id), nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
}
