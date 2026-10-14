package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
)

// A slow rule's test returns at once and keeps running after the request is
// gone, as when Cloudflare drops a request it considers too slow (HTTP 524).
// Progress is visible while it runs.
func TestTestRunIsBackgroundAndSurvivesRequestCancel(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer slow.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	srv, st := newTestServer(t, true) // dev auth: admin
	rule, err := st.CreateRule(context.Background(), rules.Rule{
		Name: "slow", Enabled: true,
		Actions: []rules.Action{
			{Type: rules.ActionDelay, DelayMillis: 1},
			{Type: rules.ActionHTTP, URL: slow.URL, TimeoutSeconds: 30},
		},
	}, store.Editor{ID: "dev"})
	if err != nil {
		t.Fatal(err)
	}

	// Start the test with a request context we cancel right after it returns.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/rules/%d/test", srv.URL, rule.ID), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Run store.RunView `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()
	cancel()
	if resp.StatusCode != http.StatusAccepted || started.Run.Status != store.RunRunning {
		t.Fatalf("start: %d %+v", resp.StatusCode, started.Run)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("test endpoint blocked for %s", time.Since(start))
	}

	get := func() store.RunView {
		resp, err := http.Get(fmt.Sprintf("%s/api/runs/%d", srv.URL, started.Run.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var run store.RunView
		_ = json.NewDecoder(resp.Body).Decode(&run)
		return run
	}

	// While blocked on the slow target: still running, first step recorded.
	deadline := time.Now().Add(5 * time.Second)
	for {
		run := get()
		if run.Status == store.RunRunning && len(run.Steps) == 1 && run.Steps[0].OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no progress: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Let the target answer: the run finishes OK despite the request being gone.
	close(release)
	deadline = time.Now().Add(5 * time.Second)
	for {
		run := get()
		if run.Status == store.RunOK && len(run.Steps) == 2 {
			break
		}
		if run.Status != store.RunRunning || time.Now().After(deadline) {
			t.Fatalf("final: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// While a rule runs, its current step (with a countdown for delays) and its
// step count are visible on the run and in /api/rules, and cleared after.
func TestRunShowsCurrentStepAndDelayCountdown(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	srv, st := newTestServer(t, true)
	rule, err := st.CreateRule(context.Background(), rules.Rule{
		Name: "progress", Enabled: true,
		Actions: []rules.Action{
			{Type: rules.ActionDelay, Name: "Settle", DelayMillis: 1500},
			{Type: rules.ActionHTTP, URL: slow.URL, TimeoutSeconds: 30},
		},
	}, store.Editor{ID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(fmt.Sprintf("%s/api/rules/%d/test", srv.URL, rule.ID), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Run store.RunView `json:"run"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&started)
	resp.Body.Close()

	get := func() store.RunView {
		resp, err := http.Get(fmt.Sprintf("%s/api/runs/%d", srv.URL, started.Run.ID))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var run store.RunView
		_ = json.NewDecoder(resp.Body).Decode(&run)
		return run
	}
	waitFor := func(what string, ok func(store.RunView) bool) store.RunView {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			run := get()
			if ok(run) {
				return run
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %+v (current %+v)", what, run, run.Current)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	delay := waitFor("delay in progress", func(r store.RunView) bool { return r.Current != nil && r.Current.Type == "delay" })
	if delay.TotalSteps != 2 || delay.Current.Index != 0 || delay.Current.Name != "Settle" ||
		delay.Current.RemainingMs == nil || *delay.Current.RemainingMs <= 0 || *delay.Current.RemainingMs > 1500 || delay.Current.Until == nil {
		t.Fatalf("delay step: total %d current %+v", delay.TotalSteps, delay.Current)
	}

	// The rule's card data shows it running.
	rresp, _ := http.Get(srv.URL + "/api/rules")
	var list []store.RuleView
	_ = json.NewDecoder(rresp.Body).Decode(&list)
	rresp.Body.Close()
	if len(list) != 1 || len(list[0].ActiveRuns) != 1 || list[0].ActiveRuns[0].Current == nil {
		t.Fatalf("active runs in /api/rules: %+v", list)
	}

	httpStep := waitFor("http step in progress", func(r store.RunView) bool { return r.Current != nil && r.Current.Index == 1 })
	if httpStep.Current.Type != "http" || httpStep.Current.Name != "HTTP request" || httpStep.Current.RemainingMs != nil || len(httpStep.Steps) != 1 {
		t.Fatalf("http step: %+v steps %d", httpStep.Current, len(httpStep.Steps))
	}

	close(release)
	done := waitFor("finished", func(r store.RunView) bool { return r.Status != store.RunRunning })
	if done.Status != store.RunOK || done.Current != nil {
		t.Fatalf("finished run: %s current %+v", done.Status, done.Current)
	}
}
