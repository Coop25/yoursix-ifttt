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
