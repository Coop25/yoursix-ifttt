package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/config"
	"yoursix-ifttt/internal/engine"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
)

const secretValue = "hunter2-super-secret"

type twoAdmins struct {
	srv          *httptest.Server
	st           *store.Store
	alice, bob   *http.Cookie
	mu           sync.Mutex
	gotPasswords []string
	gotQueries   []string
	target       *httptest.Server
}

func newTwoAdmins(t *testing.T) *twoAdmins {
	t.Helper()
	ta := &twoAdmins{}
	ta.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, _ := r.BasicAuth()
		ta.mu.Lock()
		ta.gotPasswords = append(ta.gotPasswords, pass)
		ta.gotQueries = append(ta.gotQueries, r.URL.RawQuery)
		ta.mu.Unlock()
		// Echo the query back so we can check the response gets redacted too.
		_, _ = io.WriteString(w, "you sent "+r.URL.Query().Get("k"))
	}))
	t.Cleanup(ta.target.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ta.st = st

	box, _ := secrets.NewBox(bytes.Repeat([]byte{7}, 32))
	sm := secrets.NewManager(box, st)
	cfg := config.Config{
		MaxBodyBytes: 1 << 16,
		Webhook:      config.WebhookConfig{Token: testToken},
		Auth: config.AuthConfig{
			SessionSecret:   strings.Repeat("k", 32),
			SessionDuration: time.Hour,
			AdminUserIDs:    map[string]struct{}{"alice": {}, "bob": {}},
		},
	}
	srv := New(cfg, st, engine.New(st, actions.NewRunner("", "test"), sm, log), sm, log, "test")
	ta.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(ta.srv.Close)

	mint := func(id string) *http.Cookie {
		claims, _ := json.Marshal(sessionClaims{Sub: id, Username: id, Exp: time.Now().Add(time.Hour).Unix()})
		return &http.Cookie{Name: sessionCookie, Value: srv.auth.seal(b64(claims))}
	}
	ta.alice, ta.bob = mint("alice"), mint("bob")
	return ta
}

func (ta *twoAdmins) do(t *testing.T, who *http.Cookie, method, path string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ta.srv.URL+path, rdr)
	req.AddCookie(who)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (ta *twoAdmins) ruleUsingSecret(name string) map[string]any {
	return map[string]any{
		"name": name, "enabled": true,
		"trigger": map[string]any{"codes": []string{"vmd"}},
		"actions": []map[string]any{{
			"type": "http", "auth": "basic", "username": "root",
			"password": `{{ secret "CAM_PASS" }}`,
			// Deliberately also leak it into the URL to prove redaction.
			"url": ta.target.URL + `/?k={{ secret "CAM_PASS" }}`,
		}},
	}
}

func TestSecretsAreOwnerScopedAndWriteOnly(t *testing.T) {
	ta := newTwoAdmins(t)

	if code, body := ta.do(t, ta.alice, "PUT", "/api/secrets/cam_pass", map[string]string{"value": secretValue}); code != 200 {
		t.Fatalf("alice put: %d %s", code, body)
	}

	// Alice sees the name but never the value.
	_, body := ta.do(t, ta.alice, "GET", "/api/secrets", nil)
	if !strings.Contains(body, "CAM_PASS") || strings.Contains(body, secretValue) {
		t.Fatalf("alice list: %s", body)
	}
	// Bob sees nothing of Alice's.
	if _, body := ta.do(t, ta.bob, "GET", "/api/secrets", nil); strings.Contains(body, "CAM_PASS") {
		t.Fatalf("bob can see alice's secret names: %s", body)
	}
	// Bob can't delete it or overwrite it (PUT creates his own, separate one).
	if code, _ := ta.do(t, ta.bob, "DELETE", "/api/secrets/CAM_PASS", nil); code != 404 {
		t.Fatalf("bob delete: %d", code)
	}

	// Bob can't save a rule that uses a secret he doesn't own.
	if code, body := ta.do(t, ta.bob, "POST", "/api/rules", ta.ruleUsingSecret("bob steals")); code != 400 || !strings.Contains(body, "CAM_PASS") {
		t.Fatalf("bob rule with alice's secret: %d %s", code, body)
	}

	// Alice can.
	code, body := ta.do(t, ta.alice, "POST", "/api/rules", ta.ruleUsingSecret("alice"))
	if code != 201 {
		t.Fatalf("alice rule: %d %s", code, body)
	}
	var rule struct {
		ID        int64  `json:"id"`
		UpdatedBy string `json:"updatedBy"`
	}
	_ = json.Unmarshal([]byte(body), &rule)
	if rule.UpdatedBy != "alice" {
		t.Fatalf("owner = %q", rule.UpdatedBy)
	}

	// Bob can run Alice's unchanged rule (it still runs as Alice, to Alice's
	// URL), and the value is masked in everything recorded.
	code, body = ta.do(t, ta.bob, "POST", fmt.Sprintf("/api/rules/%d/test", rule.ID), map[string]any{})
	if code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("test run: %d %s", code, body)
	}
	if strings.Contains(body, secretValue) {
		t.Fatalf("secret leaked in run result: %s", body)
	}
	ta.mu.Lock()
	if len(ta.gotPasswords) != 1 || ta.gotPasswords[0] != secretValue {
		t.Fatalf("target got passwords %v", ta.gotPasswords)
	}
	ta.mu.Unlock()
	evs, _ := ta.st.ListEvents(context.Background(), 10, 0)
	stored, _ := json.Marshal(evs)
	if strings.Contains(string(stored), secretValue) {
		t.Fatal("secret leaked into stored run history")
	}

	// Bob tries to hijack Alice's rule by editing it: rejected, because saving
	// would make it his and he has no CAM_PASS.
	hijack := ta.ruleUsingSecret("alice")
	if code, _ := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/rules/%d", rule.ID), hijack); code != 400 {
		t.Fatalf("bob hijack: %d", code)
	}

	// Client-supplied ownership is ignored (and Bob still has no CAM_PASS).
	hijack["updatedBy"] = "alice"
	if code, _ := ta.do(t, ta.bob, "PUT", fmt.Sprintf("/api/rules/%d", rule.ID), hijack); code != 400 {
		t.Fatalf("bob spoofed owner: %d", code)
	}
}

func TestSecretAtRestIsEncrypted(t *testing.T) {
	ta := newTwoAdmins(t)
	ta.do(t, ta.alice, "PUT", "/api/secrets/CAM_PASS", map[string]string{"value": secretValue})
	ct, err := ta.st.SecretCiphertext(context.Background(), "alice", "CAM_PASS")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte(secretValue)) {
		t.Fatal("plaintext stored in database")
	}
}

func TestSecretNameValidation(t *testing.T) {
	ta := newTwoAdmins(t)
	for _, bad := range []string{"1ABC", "has-dash", strings.Repeat("A", 65)} {
		if code, _ := ta.do(t, ta.alice, "PUT", "/api/secrets/"+bad, map[string]string{"value": "x"}); code != 400 {
			t.Errorf("name %q accepted: %d", bad, code)
		}
	}
	if code, _ := ta.do(t, ta.alice, "PUT", "/api/secrets/OK", map[string]string{"value": ""}); code != 400 {
		t.Errorf("empty value accepted: %d", code)
	}
}

func TestSecretsRequireAdmin(t *testing.T) {
	srv, _ := newTestServer(t, false)
	resp, _ := http.Get(srv.URL + "/api/secrets")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
}
