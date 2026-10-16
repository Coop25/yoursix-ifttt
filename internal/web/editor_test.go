package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A save based on a stale copy is refused with the current version, so the
// editor can show what changed; without updatedAt there's no check.
func TestEditConflict(t *testing.T) {
	ta := newTwoAdmins(t)
	rule := map[string]any{
		"name": "Shared", "enabled": true,
		"actions": []map[string]any{{"type": "delay", "delayMillis": 5}},
	}
	id := ta.create(t, ta.alice, "/api/rules", rule)
	path := fmt.Sprintf("/api/rules/%d", id)
	_, body := ta.do(t, ta.alice, "GET", path, nil)
	var opened struct {
		UpdatedAt string `json:"updatedAt"`
	}
	_ = json.Unmarshal([]byte(body), &opened)

	// Bob saves first.
	rule["updatedAt"] = opened.UpdatedAt
	rule["name"] = "Bob's"
	if code, body := ta.do(t, ta.bob, "PUT", path, rule); code != http.StatusOK {
		t.Fatalf("bob: %d %s", code, body)
	}
	// Alice's save from the same copy conflicts.
	rule["name"] = "Alice's"
	code, body := ta.do(t, ta.alice, "PUT", path, rule)
	if code != http.StatusConflict || !strings.Contains(body, `"conflict":true`) || !strings.Contains(body, `"name":"Bob's"`) {
		t.Fatalf("alice: %d %s", code, body)
	}
	// Overwriting: no updatedAt, no check.
	delete(rule, "updatedAt")
	if code, body := ta.do(t, ta.alice, "PUT", path, rule); code != http.StatusOK {
		t.Fatalf("overwrite: %d %s", code, body)
	}

	// Saved actions too.
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))
	sr := ta.savedRequest(false)
	sr["updatedAt"] = "2000-01-01T00:00:00Z"
	if code, body := ta.do(t, ta.alice, "PUT", fmt.Sprintf("/api/requests/%d", reqID), sr); code != http.StatusConflict {
		t.Fatalf("saved action: %d %s", code, body)
	}
}

func TestPreview(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(true))
	rule := map[string]any{
		"name":    "Preview me",
		"trigger": map[string]any{"codes": []string{"peripheral:input_activated"}},
		"actions": []map[string]any{
			{"type": "http", "url": "http://cam/{{ .data.peripheralName }}", "password": `{{ secret "CAM_PASS" }}`, "headers": map[string]string{"X-Rule": "{{ .rule }}"}},
			{"type": "discord", "message": "{{ .nope.deeper.x }}"},
			{"type": "request", "requestId": reqID, "params": map[string]string{"preset": "P-{{ .event.eventCode }}"}},
			{"type": "http", "url": "{{ if }}"},
		},
		"onFailure": []map[string]any{{"type": "discord", "message": "{{ .failure.step }} failed: {{ .failure.error }}"}},
	}
	code, body := ta.do(t, ta.alice, "POST", "/api/preview", map[string]any{"rule": rule})
	if code != http.StatusOK {
		t.Fatalf("preview: %d %s", code, body)
	}
	var got struct {
		Matches   bool `json:"matchesTrigger"`
		Actions   [][]struct{ Field, Value, Error string }
		OnFailure [][]struct{ Field, Value, Error string }
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	find := func(fields []struct{ Field, Value, Error string }, name string) (string, string) {
		for _, f := range fields {
			if f.Field == name {
				return f.Value, f.Error
			}
		}
		t.Fatalf("no field %q in %+v", name, fields)
		return "", ""
	}
	if !got.Matches {
		t.Error("sample should match its own trigger")
	}
	if v, _ := find(got.Actions[0], "URL"); v != "http://cam/Input 1" {
		t.Errorf("URL = %q", v)
	}
	if v, _ := find(got.Actions[0], "Password"); v != "‹secret CAM_PASS›" || strings.Contains(body, secretValue) {
		t.Errorf("password = %q (secrets must stay masked)", v)
	}
	if v, _ := find(got.Actions[0], "Header X-Rule"); v != "Preview me" {
		t.Errorf("header = %q", v)
	}
	if v, _ := find(got.Actions[2], "Parameter preset"); v != "P-input_activated" {
		t.Errorf("param = %q", v)
	}
	if v, _ := find(got.Actions[2], "URL"); !strings.Contains(v, "preset=P-input_activated&speed=5") {
		t.Errorf("saved action URL = %q", v)
	}
	if _, e := find(got.Actions[3], "URL"); e == "" {
		t.Error("broken template should report an error")
	}
	if v, _ := find(got.OnFailure[0], "Message"); !strings.HasPrefix(v, "Action 1 failed: sample error") {
		t.Errorf("on-failure message = %q", v)
	}
}
