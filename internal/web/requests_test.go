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

func TestSavedActionRejectsNestedWatchingDelay(t *testing.T) {
	ta := newTwoAdmins(t)
	watching := ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name": "Watcher", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "stop": true}},
	})
	calls := []map[string]any{{"type": "request", "requestId": watching}}
	const want = "can't run while another delay is watching"

	code, body := ta.do(t, ta.alice, "POST", "/api/requests", map[string]any{
		"name": "Outer", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "actions": calls}},
	})
	if code != 400 || !strings.Contains(body, want) {
		t.Fatalf("saved: %d %s", code, body)
	}

	rule := map[string]any{
		"name": "outer rule", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "ifNoMatch": calls}}},
	}
	code, body = ta.do(t, ta.alice, "POST", "/api/rules", rule)
	if code != 400 || !strings.Contains(body, want) {
		t.Fatalf("rule: %d %s", code, body)
	}
}

// A saved action a saved wait's watch runs counts as in use: deleting it is
// refused, and its "used by" names the wait.
func TestSavedActionUsedBySavedWait(t *testing.T) {
	ta := newTwoAdmins(t)
	pan := ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name": "Pan home", "request": map[string]any{"type": "http", "method": "GET", "url": "http://cam.local/home"},
	})
	ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name": "Hold on shop", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}},
				"ifNoMatch": []map[string]any{{"type": "request", "requestId": pan}}}},
	})

	code, body := ta.do(t, ta.alice, "GET", fmt.Sprintf("/api/requests/%d", pan), nil)
	if code != 200 || !strings.Contains(body, `"name":"Hold on shop","kind":"action"`) {
		t.Fatalf("get: %d %s", code, body)
	}
	code, body = ta.do(t, ta.alice, "DELETE", fmt.Sprintf("/api/requests/%d", pan), nil)
	if code != 409 || !strings.Contains(body, "saved wait(s): [Hold on shop]") {
		t.Fatalf("delete: %d %s", code, body)
	}
}

// A saved action can't start watching while something runs it where a
// watching wait can't run: another delay's watch, or on-failure steps.
func TestSavedActionCantStartWatchingUnderCallers(t *testing.T) {
	ta := newTwoAdmins(t)
	plain := map[string]any{"type": "delay", "delayMillis": 5}
	watching := map[string]any{"type": "delay", "delayMillis": 5,
		"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "stop": true}}
	update := func(id int64) (int, string) {
		return ta.do(t, ta.alice, "PUT", fmt.Sprintf("/api/requests/%d", id), map[string]any{"name": "Hold", "request": watching})
	}
	newPlain := func(name string) int64 {
		return ta.create(t, ta.alice, "/api/requests", map[string]any{"name": name, "request": plain})
	}
	call := func(id int64) []map[string]any { return []map[string]any{{"type": "request", "requestId": id}} }

	onFail := newPlain("on-failure")
	ta.create(t, ta.alice, "/api/rules", map[string]any{"name": "R1", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}}, "actions": []map[string]any{plain}, "onFailure": call(onFail)})
	if code, body := update(onFail); code != 400 || !strings.Contains(body, `rule \"R1\" runs this in \"If something goes wrong\"`) {
		t.Fatalf("on-failure: %d %s", code, body)
	}

	inRuleWatch := newPlain("in rule watch")
	ta.create(t, ta.alice, "/api/rules", map[string]any{"name": "R2", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "actions": call(inRuleWatch)}}}})
	if code, body := update(inRuleWatch); code != 400 || !strings.Contains(body, `rule \"R2\" runs this while another delay is watching`) {
		t.Fatalf("rule watch: %d %s", code, body)
	}

	inSavedWait := newPlain("in saved wait")
	ta.create(t, ta.alice, "/api/requests", map[string]any{"name": "Outer", "request": map[string]any{"type": "delay", "delayMillis": 5,
		"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "ifNoMatch": call(inSavedWait)}}})
	if code, body := update(inSavedWait); code != 400 || !strings.Contains(body, `saved wait \"Outer\" runs this while it is watching`) {
		t.Fatalf("saved wait: %d %s", code, body)
	}

	self := newPlain("self")
	code, body := ta.do(t, ta.alice, "PUT", fmt.Sprintf("/api/requests/%d", self), map[string]any{"name": "self", "request": map[string]any{"type": "delay", "delayMillis": 5,
		"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "actions": call(self)}}})
	if code != 400 || !strings.Contains(body, "can't run this saved action itself") {
		t.Fatalf("self: %d %s", code, body)
	}

	// A plain top-level use is fine.
	free := newPlain("free")
	ta.create(t, ta.alice, "/api/rules", map[string]any{"name": "R3", "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}}, "actions": call(free)})
	if code, body := update(free); code != 200 {
		t.Fatalf("top-level use: %d %s", code, body)
	}
}

// savedWaitWithSecret is a saved wait whose if-no-match action sends CAM_PASS.
func (ta *twoAdmins) savedWaitWithSecret() map[string]any {
	return map[string]any{
		"name": "Wait then call", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}},
				"ifNoMatch": []map[string]any{{"type": "http", "method": "GET", "auth": "basic", "username": "root",
					"url": ta.target.URL + "/", "password": `{{ secret "CAM_PASS" }}`}}}},
	}
}

func TestSavedWaitWatchActionSecretOwnership(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})

	// Bob has no CAM_PASS, so he can't save a wait whose watch uses it.
	if code, body := ta.do(t, ta.bob, "POST", "/api/requests", ta.savedWaitWithSecret()); code != 400 || !strings.Contains(body, "CAM_PASS") {
		t.Fatalf("bob saving a wait with alice's secret: %d %s", code, body)
	}
	wait := ta.create(t, ta.alice, "/api/requests", ta.savedWaitWithSecret())

	// Bob can't build a rule on Alice's wait: it would send her secret.
	if code, body := ta.do(t, ta.bob, "POST", "/api/rules", ruleUsing("bob", wait, nil)); code != 400 || !strings.Contains(body, "only their rules") {
		t.Fatalf("bob using alice's secret wait: %d %s", code, body)
	}
	// Alice can.
	if code, body := ta.do(t, ta.alice, "POST", "/api/rules", ruleUsing("alice", wait, nil)); code != 201 {
		t.Fatalf("alice using her wait: %d %s", code, body)
	}
	// The Secrets tab lists the wait as using CAM_PASS.
	if _, body := ta.do(t, ta.alice, "GET", "/api/secrets", nil); !strings.Contains(body, "Saved action: Wait then call") {
		t.Fatalf("secrets used-by misses the wait: %s", body)
	}

	// Secrets can't go into parameters of a saved action the watch calls.
	shared := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(false))
	viaParams := map[string]any{
		"name": "Wait then PTZ", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}},
				"actions": []map[string]any{{"type": "request", "requestId": shared,
					"params": map[string]string{"preset": `{{ secret "CAM_PASS" }}`}}}}},
	}
	if code, body := ta.do(t, ta.alice, "POST", "/api/requests", viaParams); code != 400 || !strings.Contains(body, "watch action 1: parameter preset") {
		t.Fatalf("secret in a watch action's parameters: %d %s", code, body)
	}
}

// A saved wait whose watch calls a secret-using saved action: only that
// action's owner may save it, and it then carries the action's secrets.
func TestSavedWaitCallingSecretActionOwnership(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})
	withSecret := ta.create(t, ta.alice, "/api/requests", ta.savedRequest(true))
	wait := map[string]any{
		"name": "Wait then PTZ", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}},
				"ifNoMatch": []map[string]any{{"type": "request", "requestId": withSecret,
					"params": map[string]string{"preset": "x"}}}}},
	}
	const want = `if-no-match action 1: saved action \"PTZ preset\" uses secrets belonging to alice, so only their rules and saved actions can use it`

	// Bob can't save a wait that calls Alice's secret-using action: he
	// chooses its parameters.
	if code, body := ta.do(t, ta.bob, "POST", "/api/requests", wait); code != 400 || !strings.Contains(body, want) {
		t.Fatalf("bob creating: %d %s", code, body)
	}
	plain := ta.create(t, ta.bob, "/api/requests", map[string]any{"name": "Bob's wait", "request": map[string]any{"type": "delay", "delayMillis": 5}})
	if code, body := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/requests/%d", plain), wait); code != 400 || !strings.Contains(body, want) {
		t.Fatalf("bob updating his wait: %d %s", code, body)
	}
	// Alice can, and then Bob can't edit hers.
	aliceWait := ta.create(t, ta.alice, "/api/requests", wait)
	if code, body := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/requests/%d", aliceWait), wait); code != 400 || !strings.Contains(body, want) {
		t.Fatalf("bob updating alice's wait: %d %s", code, body)
	}

	// The wait now uses Alice's secrets, so only her rules can use it (Bob
	// stands in for any third admin).
	if code, body := ta.do(t, ta.bob, "POST", "/api/rules", ruleUsing("bob", aliceWait, nil)); code != 400 || !strings.Contains(body, `saved action \"Wait then PTZ\" uses secrets belonging to alice`) {
		t.Fatalf("bob using alice's wait: %d %s", code, body)
	}
	if code, body := ta.do(t, ta.alice, "POST", "/api/rules", ruleUsing("alice", aliceWait, nil)); code != 201 {
		t.Fatalf("alice using her wait: %d %s", code, body)
	}
	if _, body := ta.do(t, ta.alice, "GET", "/api/secrets", nil); !strings.Contains(body, "Saved action: Wait then PTZ") {
		t.Fatalf("secrets used-by misses the wait: %s", body)
	}
}

func TestOnFailureRejectsWatchingSavedWait(t *testing.T) {
	ta := newTwoAdmins(t)
	watching := ta.create(t, ta.alice, "/api/requests", map[string]any{
		"name": "Watcher", "request": map[string]any{"type": "delay", "delayMillis": 5,
			"watch": map[string]any{"trigger": map[string]any{"codes": []string{"vmd"}}, "stop": true}},
	})
	plain := ta.create(t, ta.alice, "/api/requests", map[string]any{"name": "Pause", "request": map[string]any{"type": "delay", "delayMillis": 5}})
	rule := ruleUsing("on failure", watching, nil)
	rule["onFailure"] = []map[string]any{{"type": "request", "requestId": plain}, {"type": "request", "requestId": watching}}
	code, body := ta.do(t, ta.alice, "POST", "/api/rules", rule)
	want := `on-failure action 2: saved action \"Watcher\" watches for events, which steps in \"If something goes wrong\" can't do`
	if code != 400 || !strings.Contains(body, want) {
		t.Fatalf("%d %s", code, body)
	}
	// The rule's own steps can still use it.
	delete(rule, "onFailure")
	if code, body := ta.do(t, ta.alice, "POST", "/api/rules", rule); code != 201 {
		t.Fatalf("as a step: %d %s", code, body)
	}
}
