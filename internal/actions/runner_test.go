package actions

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"yoursix-ifttt/internal/mocktarget"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/yoursix"
)

func mock(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(mocktarget.New("root", "pass", slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv
}

func lastRequests(t *testing.T, base string) []mocktarget.Request {
	t.Helper()
	resp, err := http.Get(base + "/_requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []mocktarget.Request
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunnerHTTPWithTemplatesAndDigest(t *testing.T) {
	srv := mock(t)
	t.Setenv(SecretEnvPrefix+"CAM_PASS", "pass")

	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")
	acts := []rules.Action{
		{
			Type: rules.ActionHTTP, Method: "GET", Auth: "digest", Username: "root", Password: `{{ secret "cam_pass" }}`,
			URL: srv.URL + "/digest/axis-cgi/io/port.cgi?action=2:/&src={{ urlquery .data.deviceName }}",
		},
		{
			Type: rules.ActionHTTP, Method: "POST", Auth: "basic", Username: "root", Password: "pass",
			URL:     srv.URL + "/basic/notify",
			Headers: map[string]string{"X-Event": "{{ .event.key }}"},
			Body:    `{"device": {{ json .data.deviceName }}, "rule": {{ json .rule }}}`,
		},
	}
	if err := CheckTemplates(acts); err != nil {
		t.Fatal(err)
	}
	r := NewRunner("", "test")
	steps, ok := r.Run(context.Background(), acts, ev.TemplateData("Front door motion"), nil)
	if !ok {
		t.Fatalf("run failed: %+v", steps)
	}

	reqs := lastRequests(t, srv.URL)
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if reqs[0].Path != "/digest/axis-cgi/io/port.cgi" || !strings.Contains(reqs[0].Query, "src=CAM-01") {
		t.Errorf("digest request: %+v", reqs[0])
	}
	if reqs[1].Headers["X-Event"] != "device:vmd" {
		t.Errorf("header not templated: %+v", reqs[1].Headers)
	}
	if reqs[1].Body != `{"device": "CAM-01", "rule": "Front door motion"}` {
		t.Errorf("body = %s", reqs[1].Body)
	}
	if reqs[1].Headers["Content-Type"] != "application/json" {
		t.Errorf("content type = %q", reqs[1].Headers["Content-Type"])
	}
}

func TestRunnerStopsOnErrorUnlessContinue(t *testing.T) {
	srv := mock(t)
	acts := []rules.Action{
		{Type: rules.ActionHTTP, URL: srv.URL + "/digest/x", Auth: "digest", Username: "root", Password: "wrong", ContinueOnError: true},
		{Type: rules.ActionHTTP, URL: srv.URL + "/basic/x"}, // no creds -> 401, stops
		{Type: rules.ActionHTTP, URL: srv.URL + "/open"},
	}
	steps, ok := NewRunner("", "test").Run(context.Background(), acts, nil, nil)
	if ok {
		t.Fatal("expected failure")
	}
	if steps[0].OK || steps[0].Status != http.StatusUnauthorized {
		t.Errorf("step 0: %+v", steps[0])
	}
	if steps[1].OK || steps[1].Skipped {
		t.Errorf("step 1 should have run and failed: %+v", steps[1])
	}
	if !steps[2].Skipped {
		t.Errorf("step 2 should be skipped: %+v", steps[2])
	}
}

func TestSecretMissing(t *testing.T) {
	if _, err := Render(`{{ secret "definitely_not_set" }}`, nil, nil); err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestDiscordAction(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")
	_, ok := NewRunner(srv.URL, "test").Run(context.Background(),
		[]rules.Action{{Type: rules.ActionDiscord, Message: "Motion on {{ .data.deviceName }}"}}, ev.TemplateData("r"), nil)
	if !ok {
		t.Fatal("discord action failed")
	}
	if got["content"] != "Motion on CAM-01" {
		t.Fatalf("content = %v", got["content"])
	}
}

func TestHTTPErrorsAreExplained(t *testing.T) {
	// Accept connections but never answer, to force a timeout.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	refusedURL := closed.URL
	closed.Close()

	cases := []struct {
		url, want string
	}{
		{hang.URL, "no response from"},
		{refusedURL, "refused the connection"},
		{"http://definitely-not-a-real-host.invalid/", "can't resolve"},
	}
	for _, c := range cases {
		steps, ok := NewRunner("", "test").Run(context.Background(),
			[]rules.Action{{Type: rules.ActionHTTP, URL: c.url, TimeoutSeconds: 1}}, nil, nil)
		if ok || !strings.Contains(steps[0].Error, c.want) {
			t.Errorf("%s: error %q, want it to contain %q", c.url, steps[0].Error, c.want)
		}
	}
}
