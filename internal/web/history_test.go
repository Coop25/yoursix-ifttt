package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

func TestEditHistory(t *testing.T) {
	ta := newTwoAdmins(t)
	rule := map[string]any{
		"name": "History", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{"type": "delay", "delayMillis": 5}},
	}
	id := ta.create(t, ta.alice, "/api/rules", rule)
	path := fmt.Sprintf("/api/rules/%d", id)

	rule["actions"] = []map[string]any{{"type": "delay", "delayMillis": 50}}
	if code, body := ta.do(t, ta.bob, "PUT", path, rule); code != http.StatusOK {
		t.Fatalf("update: %d %s", code, body)
	}
	// Saving without changes adds nothing.
	if code, body := ta.do(t, ta.bob, "PUT", path, rule); code != http.StatusOK {
		t.Fatalf("resave: %d %s", code, body)
	}
	if code, _ := ta.do(t, ta.alice, "DELETE", path, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}

	code, body := ta.do(t, ta.alice, "GET", fmt.Sprintf("/api/history?kind=rule&id=%d", id), nil)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	var revs []store.Revision
	if err := json.Unmarshal([]byte(body), &revs); err != nil {
		t.Fatal(err)
	}
	want := []string{"deleted", "updated", "created"}
	if len(revs) != len(want) {
		t.Fatalf("got %d revisions: %s", len(revs), body)
	}
	for i, w := range want {
		if revs[i].Change != w {
			t.Errorf("revision %d: %s, want %s", i, revs[i].Change, w)
		}
	}
	if revs[1].ByID != "bob" || revs[0].ByID != "alice" {
		t.Errorf("authors: %+v", revs)
	}
	var snap map[string]any
	_ = json.Unmarshal(revs[1].Snapshot, &snap)
	if _, ok := snap["updatedAt"]; ok || snap["name"] != "History" {
		t.Errorf("snapshot should hold content only: %s", revs[1].Snapshot)
	}

	// Saved actions are logged too, under kind "action".
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))
	_, body = ta.do(t, ta.alice, "GET", "/api/history?kind=action", nil)
	_ = json.Unmarshal([]byte(body), &revs)
	if len(revs) != 1 || revs[0].ItemID != reqID || revs[0].Change != "created" {
		t.Fatalf("action history: %s", body)
	}
}

func TestEventSearch(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.create(t, ta.alice, "/api/rules", map[string]any{
		"name": "Gate opener", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{"type": "delay", "delayMillis": 1}},
	})
	for _, ev := range []yoursix.Event{yoursix.Sample("device", "vmd"), yoursix.Sample("alarmgroup", "armed")} {
		ev.EventID = ""
		ev.EventData["deviceName"] = "CAM_" + ev.EventCode
		ev.EventData["clipUrl"] = "https://clips.example/" + ev.EventCode
		if code, body := ta.do(t, ta.alice, "POST", "/api/simulate", ev); code != http.StatusOK {
			t.Fatalf("simulate: %d %s", code, body)
		}
	}

	cases := map[string]int{
		"?q=":                      2,
		"?q=device:vmd":            1,
		"?q=GATE":                  1, // rule name, case-insensitive
		"?q=cam_armed":             1, // payload
		"?q=cam%25":                0, // % is literal
		"?q=hq+armed":              1, // every word must match
		"?outcome=matched":         1,
		"?outcome=nomatch":         1,
		"?outcome=test":            2,
		"?q=armed&outcome=matched": 0,
		// Field searches.
		"?q=device:cam_vmd":                1,
		"?q=site:hq":                       2,
		"?q=site:%22Head+office%22":        0,
		"?q=rule:gate":                     1,
		"?q=code:armed":                    1,
		"?q=groupName:" + "cf84":           0, // eventData fields by name
		"?q=status:ok":                     1,
		"?q=ip:simulate":                   2,
		"?q=%22alarmgroup:armed+event%22":  1, // quoted phrase
		"?q=https://clips.example/vmd":     1, // a URL is free text, not key "https"
		"?q=https://clips.example":         2,
		"?since=2000-01-01T00:00:00Z":      2,
		"?until=2000-01-01T00:00:00Z":      0,
		"?q=hq&since=2999-01-01T00:00:00Z": 0,
	}
	for qs, want := range cases {
		code, body := ta.do(t, ta.alice, "GET", "/api/events"+qs, nil)
		var evs []store.EventView
		_ = json.Unmarshal([]byte(body), &evs)
		if code != http.StatusOK || len(evs) != want {
			t.Errorf("%s: %d, %d events, want %d", qs, code, len(evs), want)
		}
	}
	if code, _ := ta.do(t, ta.alice, "GET", "/api/events?outcome=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("bad outcome: %d", code)
	}
}
