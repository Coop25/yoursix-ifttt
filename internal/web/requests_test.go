package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func (ta *twoAdmins) savedRequest(withSecret bool) map[string]any {
	req := map[string]any{
		"method": "GET",
		"url":    ta.target.URL + "/ptz?preset={{ urlquery .params.preset }}&speed={{ .params.speed }}&cam={{ .data.deviceName }}",
	}
	if withSecret {
		req["auth"] = "basic"
		req["username"] = "root"
		req["password"] = `{{ secret "CAM_PASS" }}`
	}
	return map[string]any{
		"name": "PTZ preset",
		"params": []map[string]string{
			{"name": "preset", "description": "Preset name"},
			{"name": "speed", "default": "5"},
		},
		"request": req,
	}
}

func ruleUsing(name string, requestID int64, params map[string]string) map[string]any {
	return map[string]any{
		"name": name, "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{"type": "request", "requestId": requestID, "params": params}},
	}
}

func (ta *twoAdmins) create(t *testing.T, who *http.Cookie, path string, payload any) int64 {
	t.Helper()
	code, body := ta.do(t, who, "POST", path, payload)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var v struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &v)
	return v.ID
}

func (ta *twoAdmins) runTest(t *testing.T, who *http.Cookie, path string, body any) string {
	t.Helper()
	code, out := ta.do(t, who, "POST", path, body)
	if code != http.StatusAccepted {
		t.Fatalf("%s: %d %s", path, code, out)
	}
	var started struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	_ = json.Unmarshal([]byte(out), &started)
	return ta.waitRun(t, who, started.Run.ID)
}

func TestSavedRequestRunsWithParams(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(true))

	ruleID := ta.create(t, ta.alice, "/api/rules", ruleUsing("alice ptz", reqID, map[string]string{"preset": "Front {{ .data.siteName }}"}))
	run := ta.runTest(t, ta.alice, fmt.Sprintf("/api/rules/%d/test", ruleID), map[string]any{})
	if !strings.Contains(run, `"status":"ok"`) || !strings.Contains(run, `"name":"PTZ preset"`) {
		t.Fatalf("run: %s", run)
	}
	ta.mu.Lock()
	q, pass := ta.gotQueries[len(ta.gotQueries)-1], ta.gotPasswords[len(ta.gotPasswords)-1]
	ta.mu.Unlock()
	// Param templated with event data, default applied, event data in URL.
	if q != "preset=Front+HQ&speed=5&cam=CAM-01" || pass != secretValue {
		t.Fatalf("target got query %q password %q", q, pass)
	}

	// The saved request's own Test button, with a parameter.
	run = ta.runTest(t, ta.alice, fmt.Sprintf("/api/requests/%d/test", reqID), map[string]any{"params": map[string]string{"preset": "Gate", "speed": "9"}})
	if !strings.Contains(run, `"status":"ok"`) {
		t.Fatalf("request test: %s", run)
	}
	ta.mu.Lock()
	q = ta.gotQueries[len(ta.gotQueries)-1]
	ta.mu.Unlock()
	if q != "preset=Gate&speed=9&cam=CAM-01" {
		t.Fatalf("request test query %q", q)
	}
}

func TestSavedDelayAndDiscordActions(t *testing.T) {
	ta := newTwoAdmins(t)
	delayID := ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name": "Settle", "request": map[string]any{"type": "delay", "delayMillis": 5},
	})
	discordID := ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name":    "Alert channel",
		"params":  []map[string]string{{"name": "who", "default": "everyone"}},
		"request": map[string]any{"type": "discord", "webhookUrl": ta.target.URL + "/hook?to={{ urlquery .params.who }}", "message": "{{ .data.deviceName }}"},
	})
	rule := map[string]any{
		"name": "saved steps", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{
			{"type": "request", "requestId": delayID},
			{"type": "request", "requestId": discordID, "params": map[string]string{"who": "ops"}},
		},
	}
	ruleID := ta.create(t, ta.alice, "/api/rules", rule)
	run := ta.runTest(t, ta.alice, fmt.Sprintf("/api/rules/%d/test", ruleID), map[string]any{})
	if !strings.Contains(run, `"status":"ok"`) || !strings.Contains(run, `"name":"Settle"`) || !strings.Contains(run, `"name":"Alert channel"`) {
		t.Fatalf("run: %s", run)
	}
	ta.mu.Lock()
	q := ta.gotQueries[len(ta.gotQueries)-1]
	ta.mu.Unlock()
	if q != "to=ops" {
		t.Fatalf("discord hook got query %q", q)
	}

	// A saved action can't wrap another saved action.
	code, body := ta.do(t, ta.alice, "POST", "/api/requests", map[string]any{
		"name": "nested", "request": map[string]any{"type": "request", "requestId": delayID},
	})
	if code != 400 || !strings.Contains(body, "can't be of type") {
		t.Fatalf("nested: %d %s", code, body)
	}
}

func TestSavedRequestParamValidation(t *testing.T) {
	ta := newTwoAdmins(t)
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})

	bad := map[string]map[string]string{
		"needs a value":          {},                                    // preset required
		"has no parameter":       {"preset": "x", "zoom": "2"},          // unknown
		"can't be used in param": {"preset": `{{ secret "CAM_PASS" }}`}, // secrets in params
		`a value for \"preset`:   {"preset": ""},                        // empty required
	}
	for want, params := range bad {
		code, body := ta.do(t, ta.alice, "POST", "/api/rules", ruleUsing("bad", reqID, params))
		if code != 400 || !strings.Contains(body, want) {
			t.Errorf("params %v: %d %s (want %q)", params, code, body, want)
		}
	}
	if code, body := ta.do(t, ta.alice, "POST", "/api/rules", ruleUsing("missing", 9999, map[string]string{"preset": "x"})); code != 400 || !strings.Contains(body, "doesn't exist") {
		t.Errorf("missing request: %d %s", code, body)
	}
}

func TestSavedRequestSecretOwnership(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})
	withSecret := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(true))
	shared := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))

	// Bob can't build a rule on Alice's secret-using request: its parameters
	// could redirect her password to him.
	code, body := ta.do(t, ta.bob, "POST", "/api/rules", ruleUsing("bob", withSecret, map[string]string{"preset": "x"}))
	if code != 400 || !strings.Contains(body, "only their rules") {
		t.Fatalf("bob using alice's secret request: %d %s", code, body)
	}
	// ...but a request without secrets is shared.
	if code, body := ta.do(t, ta.bob, "POST", "/api/rules", ruleUsing("bob shared", shared, map[string]string{"preset": "x"})); code != 201 {
		t.Fatalf("bob using shared request: %d %s", code, body)
	}
	// Bob can't take over Alice's request: he doesn't have CAM_PASS.
	if code, _ := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/requests/%d", withSecret), ta.savedRequest(true)); code != 400 {
		t.Fatalf("bob editing alice's request: %d", code)
	}

	// If Bob does own a CAM_PASS and re-saves the request, it becomes his;
	// Alice's rule then refuses to run it rather than use Bob's request.
	aliceRule := ta.create(t, ta.alice, "/api/rules", ruleUsing("alice", withSecret, map[string]string{"preset": "x"}))
	ta.do(t, ta.bob, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": "bobs-own-password"})
	if code, body := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/requests/%d", withSecret), ta.savedRequest(true)); code != 200 {
		t.Fatalf("bob re-saving with his own secret: %d %s", code, body)
	}
	run := ta.runTest(t, ta.alice, fmt.Sprintf("/api/rules/%d/test", aliceRule), map[string]any{})
	if !strings.Contains(run, `"status":"failed"`) || !strings.Contains(run, "only their rules") {
		t.Fatalf("alice's rule after takeover: %s", run)
	}
	ta.mu.Lock()
	for _, p := range ta.gotPasswords {
		if p == secretValue {
			t.Error("alice's secret reached the target after takeover")
		}
	}
	ta.mu.Unlock()
}

func TestSavedRequestDeleteInUse(t *testing.T) {
	ta := newTwoAdmins(t)
	reqID := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))
	ruleID := ta.create(t, ta.alice, "/api/rules", ruleUsing("user", reqID, map[string]string{"preset": "x"}))
	code, body := ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/requests/%d", reqID), nil)
	if code != http.StatusConflict || !strings.Contains(body, `"name":"user"`) {
		t.Fatalf("delete in use: %d %s", code, body)
	}
	ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/rules/%d", ruleID), nil)
	if code, _ := ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/requests/%d", reqID), nil); code != http.StatusNoContent {
		t.Fatalf("delete unused: %d", code)
	}
}
