package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

const testToken = "test-token-0123456789abcdef"

func newTestServer(t *testing.T, devAuth bool) (*httptest.Server, *store.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
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
	eng := engine.New(st, actions.NewRunner("", "test"), sm, log)
	srv := httptest.NewServer(New(cfg, st, eng, sm, log, "test").Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func postHook(t *testing.T, url, ua string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestWebhookEndToEnd(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("cam") != "CAM-01" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
	}))
	defer target.Close()

	srv, st := newTestServer(t, false)
	ctx := t.Context()
	_, err := st.CreateRule(ctx, rules.Rule{
		Name: "motion", Enabled: true,
		Trigger: rules.Trigger{Topics: []string{"device"}, Codes: []string{"vmd"}},
		Actions: []rules.Action{{Type: rules.ActionHTTP, URL: target.URL + "/?cam={{ .data.deviceName }}"}},
	}, store.Editor{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.CreateRule(ctx, rules.Rule{
		Name: "other", Enabled: true,
		Trigger: rules.Trigger{Codes: []string{"connected"}},
		Actions: []rules.Action{{Type: rules.ActionHTTP, URL: target.URL}},
	}, store.Editor{})

	body, _ := json.Marshal(yoursix.Sample("device", "vmd"))

	if r := postHook(t, srv.URL+"/hook/wrong-token-0123456789abcdef", yoursix.UserAgent, body); r.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	if r := postHook(t, srv.URL+"/hook/"+testToken, "curl/8", body); r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong UA: %d", r.StatusCode)
	}
	if r := postHook(t, srv.URL+"/hook/"+testToken, yoursix.UserAgent, []byte(`{"nope":1}`)); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad payload: %d", r.StatusCode)
	}
	// The real header YourSix sends has the token last.
	if r := postHook(t, srv.URL+"/hook/"+testToken, "postoffice/v2.21.0 y6-platform", body); r.StatusCode != http.StatusOK {
		t.Fatalf("valid: %d", r.StatusCode)
	}
	// Redelivery of the same eventId must not fire again.
	if r := postHook(t, srv.URL+"/hook/"+testToken, yoursix.UserAgent, body); r.StatusCode != http.StatusOK {
		t.Fatalf("duplicate: %d", r.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		evs, err := st.ListEvents(ctx, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 1 && len(evs[0].Runs) == 1 && evs[0].Runs[0].Status == store.RunOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not complete: %+v", evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatalf("target hits = %d, want 1", hits.Load())
	}
}

func TestAPIRequiresLogin(t *testing.T) {
	srv, _ := newTestServer(t, false)
	for _, p := range []string{"/api/me", "/api/rules", "/api/events"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	// Forged cookie.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "eyJzdWIiOiIxIn0.forged"})
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("forged cookie accepted: %d", resp.StatusCode)
	}
}

func TestDevAuthRefusesTunnelTraffic(t *testing.T) {
	srv, _ := newTestServer(t, true)
	resp, _ := http.Get(srv.URL + "/api/me")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local dev auth: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/me", nil)
	req.Header.Set("Cf-Ray", "abc")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tunnelled dev auth should be refused: %d", resp.StatusCode)
	}
}

func TestCrossOriginMutationBlocked(t *testing.T) {
	srv, _ := newTestServer(t, true)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/rules", strings.NewReader(`{}`))
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a := newAuth(config.AuthConfig{SessionSecret: strings.Repeat("s", 32), SessionDuration: time.Hour, AdminUserIDs: map[string]struct{}{"42": {}}}, false)
	claims, _ := json.Marshal(sessionClaims{Sub: "42", Username: "frank", Exp: time.Now().Add(time.Hour).Unix()})
	rec := httptest.NewRecorder()
	a.setCookie(rec, sessionCookie, a.seal(b64(claims)), time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	u, ok := a.userFromRequest(req)
	if !ok || u.Role != RoleAdmin || u.ID != "42" {
		t.Fatalf("session not accepted: %+v %v", u, ok)
	}

	// Removing the user from the allow list revokes the session.
	delete(a.cfg.AdminUserIDs, "42")
	if _, ok := a.userFromRequest(req); ok {
		t.Fatal("revoked user still accepted")
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
