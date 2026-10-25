package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

// recorder is a target that answers at once and remembers each request's
// "msg" query value.
type recorder struct {
	mu   sync.Mutex
	msgs []string
	url  string
}

func newRecorder(t *testing.T) *recorder {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.msgs = append(rec.msgs, r.URL.Query().Get("msg"))
		rec.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	rec.url = srv.URL
	return rec
}

func (rec *recorder) got() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.msgs...)
}

func (h *harness) create(r rules.Rule) rules.Rule {
	h.t.Helper()
	r.Enabled = true
	if r.Name == "" {
		r.Name = "watch"
	}
	created, err := h.st.CreateRule(context.Background(), r, store.Editor{})
	if err != nil {
		h.t.Fatal(err)
	}
	return created.Rule
}

// send dispatches ev (given a fresh ID) and returns its event row.
func (h *harness) send(ev yoursix.Event) int64 {
	h.t.Helper()
	ev.EventID = time.Now().Format(time.RFC3339Nano)
	raw, _ := json.Marshal(ev)
	res, err := h.eng.Dispatch(context.Background(), ev, raw, "test", false)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.EventRow
}

func (h *harness) run(row int64, ruleID int64) store.RunView {
	h.t.Helper()
	h.waitIdle(ruleID)
	e, err := h.st.GetEvent(context.Background(), row)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, r := range e.Runs {
		if r.RuleID == ruleID {
			return r
		}
	}
	h.t.Fatalf("no run of rule %d on event %d: %+v", ruleID, row, e.Runs)
	return store.RunView{}
}

func otherCamera(code string) yoursix.Event {
	ev := yoursix.Sample("device", code)
	data := map[string]any{}
	for k, v := range ev.EventData {
		data[k] = v
	}
	data["deviceName"] = "CAM-02"
	ev.EventData = data
	return ev
}

// Matching events restart the countdown (keep the light on while there's
// motion); the event still goes through normal matching, so with "ignore
// while running" it's logged as busy.
func TestWatchRestartsDelay(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder(t)
	r := h.create(rules.Rule{SkipIfRunning: true, Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{
			{Type: rules.ActionDelay, DelayMillis: 300, Watch: &rules.Watch{SameAsTrigger: true, Restart: true, MaxMillis: 5000}},
			{Type: rules.ActionHTTP, URL: rec.url + "/?msg=off+{{ .watch.matches }}+{{ .watch.restarts }}"},
		}})

	start := time.Now()
	row := h.send(yoursix.Sample("device", "vmd"))
	for range 2 {
		time.Sleep(150 * time.Millisecond)
		busy := h.send(yoursix.Sample("device", "vmd"))
		if e, _ := h.st.GetEvent(context.Background(), busy); len(e.Runs) != 1 || e.Runs[0].Status != store.RunBusy {
			t.Fatalf("second event runs: %+v", e.Runs)
		}
	}
	run := h.run(row, r.ID)
	if took := time.Since(start); took < 550*time.Millisecond {
		t.Fatalf("run took %s; the countdown wasn't restarted", took)
	}
	w := run.Steps[0].Watch
	if run.Status != store.RunOK || w == nil || w.Matches != 2 || w.Restarts != 2 {
		t.Fatalf("run: %+v watch: %+v", run, w)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "off 2 2" {
		t.Fatalf("target got %q", got)
	}
}

// Restarts never push the wait past the longest wait.
func TestWatchRestartLimit(t *testing.T) {
	h := newHarness(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}}, SkipIfRunning: true,
		Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 200, Watch: &rules.Watch{SameAsTrigger: true, Restart: true, MaxMillis: 300}}}})
	row := h.send(yoursix.Sample("device", "vmd"))
	for range 6 {
		time.Sleep(60 * time.Millisecond)
		h.send(yoursix.Sample("device", "vmd"))
	}
	run := h.run(row, r.ID)
	s := run.Steps[0]
	if s.Watch == nil || s.Watch.Restarts == 0 || s.DurationMs < 300 || s.DurationMs > 380 {
		t.Fatalf("delay took %dms (want ~300, its limit), watch %+v", s.DurationMs, s.Watch)
	}
}

// Filter values are templated with the run's event, so a watch can follow
// the camera that started the run.
func TestWatchFilterTemplates(t *testing.T) {
	h := newHarness(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}}, SkipIfRunning: true,
		Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 300, Watch: &rules.Watch{SameAsTrigger: true,
			Trigger: rules.Trigger{Filters: []rules.Filter{{Field: "deviceName", Op: rules.OpEquals, Value: "{{ .data.deviceName }}"}}},
			Stop:    true}}}})
	row := h.send(yoursix.Sample("device", "vmd"))
	time.Sleep(50 * time.Millisecond)
	h.send(otherCamera("vmd")) // another camera: ignored
	time.Sleep(50 * time.Millisecond)
	h.send(yoursix.Sample("device", "vmd")) // same camera: stops the wait
	run := h.run(row, r.ID)
	if w := run.Steps[0].Watch; w == nil || w.Matches != 1 || !w.Stopped {
		t.Fatalf("watch: %+v", w)
	}
}

// On a match, the watch's actions run (seeing the matched event as .watch),
// and Stop skips the rule's remaining steps without failing the run.
func TestWatchActionsAndStop(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{
			{Type: rules.ActionDelay, DelayMillis: 2000, Watch: &rules.Watch{
				Trigger: rules.Trigger{Codes: []string{"aoa"}},
				Actions: []rules.Action{{Type: rules.ActionHTTP, URL: rec.url + "/?msg=also+{{ .watch.data.deviceName }}+{{ .data.deviceName }}"}},
				Stop:    true,
			}},
			{Type: rules.ActionHTTP, URL: rec.url + "/?msg=after"},
		}})
	row := h.send(yoursix.Sample("device", "vmd"))
	time.Sleep(50 * time.Millisecond)
	h.send(otherCamera("aoa"))
	run := h.run(row, r.ID)
	if run.Status != store.RunOK || !run.Steps[1].Skipped {
		t.Fatalf("run: %+v", run)
	}
	w := run.Steps[0].Watch
	if w == nil || len(w.Steps) != 1 || !w.Steps[0].OK || w.Steps[0].Match != 1 {
		t.Fatalf("watch: %+v", w)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "also CAM-02 CAM-01" {
		t.Fatalf("target got %q", got)
	}
}

// Actions run on the first match only, unless "every match" is on.
func TestWatchEveryMatch(t *testing.T) {
	for _, every := range []bool{false, true} {
		h := newHarness(t)
		rec := newRecorder(t)
		r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}},
			Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 400, Watch: &rules.Watch{
				Trigger:    rules.Trigger{Codes: []string{"aoa"}},
				Actions:    []rules.Action{{Type: rules.ActionHTTP, URL: rec.url + "/?msg={{ .watch.matches }}"}},
				EveryMatch: every,
			}}}})
		row := h.send(yoursix.Sample("device", "vmd"))
		for range 3 {
			time.Sleep(30 * time.Millisecond)
			h.send(yoursix.Sample("device", "aoa"))
		}
		h.run(row, r.ID)
		want := 1
		if every {
			want = 3
		}
		if got := rec.got(); len(got) != want {
			t.Fatalf("every=%v: target got %q", every, got)
		}
	}
}

// If nothing matches, the if-no-match actions run.
func TestWatchIfNoMatch(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 50, Watch: &rules.Watch{
			Trigger:   rules.Trigger{Codes: []string{"aoa"}},
			IfNoMatch: []rules.Action{{Type: rules.ActionHTTP, URL: rec.url + "/?msg=quiet+{{ .watch.matches }}"}},
		}}}})
	run := h.run(h.send(yoursix.Sample("device", "vmd")), r.ID)
	w := run.Steps[0].Watch
	if run.Status != store.RunOK || w == nil || w.Matches != 0 || len(w.Steps) != 1 || w.Steps[0].Match != 0 {
		t.Fatalf("run: %+v watch: %+v", run, w)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "quiet 0" {
		t.Fatalf("target got %q", got)
	}
}

// A failing watch action is recorded and fails the run, but the wait and
// the rule carry on, and on-failure actions don't run (like continue on
// error).
func TestWatchActionFailure(t *testing.T) {
	h := newHarness(t)
	rec := newRecorder(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{
			{Type: rules.ActionDelay, DelayMillis: 50, Watch: &rules.Watch{
				Trigger:   rules.Trigger{Codes: []string{"aoa"}},
				IfNoMatch: []rules.Action{{Type: rules.ActionHTTP, URL: "http://127.0.0.1:1/x", TimeoutSeconds: 2}},
			}},
			{Type: rules.ActionHTTP, URL: rec.url + "/?msg=after"},
		},
		OnFailure: []rules.Action{{Type: rules.ActionHTTP, URL: rec.url + "/?msg=alert"}}})
	run := h.run(h.send(yoursix.Sample("device", "vmd")), r.ID)
	if run.Status != store.RunFailed || !run.Steps[0].OK || !run.Steps[1].OK || len(run.Steps) != 2 {
		t.Fatalf("run: %+v", run)
	}
	if w := run.Steps[0].Watch; w == nil || len(w.Steps) != 1 || w.Steps[0].OK {
		t.Fatalf("watch: %+v", w)
	}
	if got := rec.got(); len(got) != 1 || got[0] != "after" {
		t.Fatalf("target got %q", got)
	}
}

// A finished watch stops listening.
func TestWatchUnsubscribes(t *testing.T) {
	h := newHarness(t)
	r := h.create(rules.Rule{Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{{Type: rules.ActionDelay, DelayMillis: 20, Watch: &rules.Watch{SameAsTrigger: true, Stop: true}}}})
	h.run(h.send(yoursix.Sample("device", "vmd")), r.ID)
	h.eng.mu.Lock()
	n := len(h.eng.watchers)
	h.eng.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d watchers left", n)
	}
}
