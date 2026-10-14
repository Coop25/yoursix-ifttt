package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/config"
	"yoursix-ifttt/internal/engine"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

func newLiveServer(t *testing.T, devAuth bool) (*httptest.Server, *Server, *store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		MaxBodyBytes: 1 << 16,
		Webhook:      config.WebhookConfig{Token: testToken, RequireUserAgent: true},
		Auth:         config.AuthConfig{AdminUserIDs: map[string]struct{}{"1": {}}},
		DevAuth:      devAuth,
	}
	sm := secrets.NewManager(nil, st)
	srv := New(cfg, st, engine.New(st, actions.NewRunner("", "test"), sm, log), sm, log, "test")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return ts, srv, st
}

// openStream connects to the event stream and returns its lines.
func openStream(t *testing.T, url string) (<-chan string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/api/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines, func() { cancel(); resp.Body.Close() }
}

// nextEvent waits for the next pushed event that satisfies ok.
func nextEvent(t *testing.T, lines <-chan string, ok func(store.EventView) bool) store.EventView {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatal("stream closed")
			}
			data, found := strings.CutPrefix(line, "data: ")
			if !found {
				continue
			}
			var ev store.EventView
			if json.Unmarshal([]byte(data), &ev) == nil && ev.ID > 0 && ok(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for a pushed event")
		}
	}
}

func TestLiveStreamPushesEventsAndRuns(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	ts, _, st := newLiveServer(t, true)
	if _, err := st.CreateRule(context.Background(), rules.Rule{
		Name: "motion", Enabled: true,
		Trigger: rules.Trigger{Codes: []string{"vmd"}},
		Actions: []rules.Action{{Type: rules.ActionHTTP, URL: target.URL}},
	}, store.Editor{ID: "dev"}); err != nil {
		t.Fatal(err)
	}

	lines, stop := openStream(t, ts.URL)
	defer stop()

	body, _ := json.Marshal(yoursix.Sample("device", "vmd"))
	if r := postHook(t, ts.URL+"/hook/"+testToken, yoursix.UserAgent, body); r.StatusCode != 200 {
		t.Fatalf("webhook: %d", r.StatusCode)
	}

	// The event is pushed with its rule run finished OK. Updates are batched,
	// so a fast run may arrive as one message or as several (received,
	// running, done); wait for the finished state either way.
	done := nextEvent(t, lines, func(ev store.EventView) bool {
		return ev.Code == "vmd" && len(ev.Runs) == 1 && ev.Runs[0].Status == store.RunOK
	})
	if len(done.Payload) != 0 {
		t.Fatalf("payload pushed: %s", done.Payload)
	}
	if done.Runs[0].RuleName != "motion" {
		t.Fatalf("run: %+v", done.Runs[0])
	}
}

func TestLiveStreamRequiresLogin(t *testing.T) {
	ts, _, _ := newLiveServer(t, false)
	resp, err := http.Get(ts.URL + "/api/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous stream: %d", resp.StatusCode)
	}
}

func TestLiveStreamEndsOnShutdown(t *testing.T) {
	ts, srv, _ := newLiveServer(t, true)
	lines, stop := openStream(t, ts.URL)
	defer stop()
	srv.Close()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-lines:
			if !open {
				return // stream ended
			}
		case <-deadline:
			t.Fatal("stream still open after shutdown")
		}
	}
}

func TestEventsCarryMatrixLink(t *testing.T) {
	ts, _, _ := newLiveServer(t, true)
	lines, stop := openStream(t, ts.URL)
	defer stop()

	cam := yoursix.Sample("device", "vmd") // videoSourceId "0"
	cam.EventOccurred = "2026-10-14T16:43:17.084Z"
	body, _ := json.Marshal(cam)
	postHook(t, ts.URL+"/hook/"+testToken, yoursix.UserAgent, body)
	group := yoursix.Sample("alarmgroup", "armed")
	body, _ = json.Marshal(group)
	postHook(t, ts.URL+"/hook/"+testToken, yoursix.UserAgent, body)

	wantCam := "https://platform.yoursix.com/matrix?deviceIds=" + cam.EventData["deviceId"].(string) + "_0&timestamp=1791996197.084"
	wantGroup := "https://platform.yoursix.com/matrix?alarmGroupId=" + group.EventData["groupId"].(string) + "&timestamp="

	// Pushed live...
	pushed := nextEvent(t, lines, func(ev store.EventView) bool { return ev.Code == "vmd" })
	if pushed.MatrixURL != wantCam {
		t.Fatalf("pushed link:\n got %s\nwant %s", pushed.MatrixURL, wantCam)
	}
	// ...and in the list.
	resp, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	var list []store.EventView
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	got := map[string]string{}
	for _, ev := range list {
		got[ev.Code] = ev.MatrixURL
	}
	if got["vmd"] != wantCam || !strings.HasPrefix(got["armed"], wantGroup) {
		t.Fatalf("list links: %v", got)
	}
}
