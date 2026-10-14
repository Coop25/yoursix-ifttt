package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

const ran = "ran"

// harness wires an engine to a target whose handler blocks until released,
// so tests control exactly when a run is "still going".
type harness struct {
	t       *testing.T
	eng     *Engine
	st      *store.Store
	hits    atomic.Int32
	release chan struct{}
	url     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, release: make(chan struct{})}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		<-h.release
	}))
	t.Cleanup(target.Close)
	t.Cleanup(func() {
		// Unblock any handler still waiting so the server can close.
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	})
	h.url = target.URL

	st, err := store.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	h.eng = New(st, actions.NewRunner("", "test"), secrets.NewManager(nil, st), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

func (h *harness) rule(r rules.Rule) rules.Rule {
	h.t.Helper()
	r.Name, r.Enabled = "r", true
	r.Trigger = rules.Trigger{Codes: []string{"vmd"}}
	r.Actions = []rules.Action{{Type: rules.ActionHTTP, URL: h.url, TimeoutSeconds: 30}}
	created, err := h.st.CreateRule(context.Background(), r, store.Editor{})
	if err != nil {
		h.t.Fatal(err)
	}
	return created.Rule
}

// fire dispatches a fresh event and returns the status recorded for the rule.
func (h *harness) fire() string {
	h.t.Helper()
	ev := yoursix.Sample("device", "vmd")
	raw, _ := json.Marshal(ev)
	res, err := h.eng.Dispatch(context.Background(), ev, raw, "test", false)
	if err != nil {
		h.t.Fatal(err)
	}
	got, err := h.st.GetEvent(context.Background(), res.EventRow)
	if err != nil || len(got.Runs) != 1 {
		h.t.Fatalf("runs: %+v %v", got.Runs, err)
	}
	// A started run may already have finished; both count as "ran".
	if s := got.Runs[0].Status; s == store.RunOK || s == store.RunRunning {
		return ran
	}
	return got.Runs[0].Status
}

func (h *harness) waitIdle(ruleID int64) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.eng.Running(ruleID) > 0 {
		if time.Now().After(deadline) {
			h.t.Fatal("run never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) waitHits(n int32) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.hits.Load() < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("hits = %d, want %d", h.hits.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSkipIfRunning(t *testing.T) {
	h := newHarness(t)
	r := h.rule(rules.Rule{SkipIfRunning: true})

	if s := h.fire(); s != ran {
		t.Fatalf("first = %s", s)
	}
	h.waitHits(1)
	// Still blocked in the target: further events are accepted but ignored.
	for range 3 {
		if s := h.fire(); s != store.RunBusy {
			t.Fatalf("while running = %s, want busy", s)
		}
	}
	close(h.release)
	h.waitIdle(r.ID)

	if s := h.fire(); s != ran {
		t.Fatalf("after finish = %s", s)
	}
	h.waitIdle(r.ID)
	if got := h.hits.Load(); got != 2 {
		t.Fatalf("target hits = %d, want 2", got)
	}
}

func TestWithoutSkipIfRunningRunsOverlap(t *testing.T) {
	h := newHarness(t)
	h.rule(rules.Rule{})
	h.fire()
	h.fire()
	h.waitHits(2) // both reached the target concurrently
}

func TestCooldownFromStart(t *testing.T) {
	h := newHarness(t)
	close(h.release) // runs complete immediately
	r := h.rule(rules.Rule{CooldownSeconds: 60})

	if s := h.fire(); s != ran {
		t.Fatalf("first = %s", s)
	}
	h.waitIdle(r.ID)
	if s := h.fire(); s != store.RunCooldown {
		t.Fatalf("second = %s, want cooldown", s)
	}

	// Pretend the window elapsed.
	h.eng.mu.Lock()
	h.eng.lastFired[r.ID] = time.Now().Add(-61 * time.Second)
	h.eng.mu.Unlock()
	if s := h.fire(); s != ran {
		t.Fatalf("after window = %s", s)
	}
}

func TestCooldownFromFinish(t *testing.T) {
	h := newHarness(t)
	r := h.rule(rules.Rule{CooldownSeconds: 60, CooldownFrom: rules.CooldownFromFinish})

	h.fire()
	h.waitHits(1)
	// While running, the window hasn't opened yet: ignored.
	if s := h.fire(); s != store.RunCooldown {
		t.Fatalf("while running = %s, want cooldown", s)
	}
	close(h.release)
	h.waitIdle(r.ID)
	// Just finished: inside the window measured from the finish.
	if s := h.fire(); s != store.RunCooldown {
		t.Fatalf("just finished = %s, want cooldown", s)
	}

	h.eng.mu.Lock()
	h.eng.lastFinished[r.ID] = time.Now().Add(-61 * time.Second)
	// Even with an old start time, the start must not be what's measured.
	h.eng.lastFired[r.ID] = time.Now()
	h.eng.mu.Unlock()
	if s := h.fire(); s != ran {
		t.Fatalf("after window = %s", s)
	}
}

func TestRuleSettingsPersist(t *testing.T) {
	h := newHarness(t)
	r := h.rule(rules.Rule{CooldownSeconds: 5, CooldownFrom: rules.CooldownFromFinish, SkipIfRunning: true})
	got, err := h.st.GetRule(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SkipIfRunning || got.CooldownFrom != rules.CooldownFromFinish || got.CooldownSeconds != 5 {
		t.Fatalf("not persisted: %+v", got.Rule)
	}
}
