package actions

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestDiscordOptionsAndEmbeds(t *testing.T) {
	var got map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")
	a := rules.Action{Type: rules.ActionDiscord, Message: "Motion", Discord: &rules.DiscordOptions{
		Username: "{{ .data.siteName }} cams", Silent: true, ThreadID: "123456789012345678",
		PingUsers: []string{"<@111111111111111111>"}, PingRoles: []string{"222222222222222222"},
		Embeds: []rules.Embed{{
			Title: "{{ .data.deviceName }}", Color: "#ff0000", Timestamp: "{{ .event.eventOccurred }}",
			FooterText: "YourSix",
			Fields:     []rules.EmbedField{{Name: "Site", Value: "{{ .data.siteName }}", Inline: true}, {Name: "Blank", Value: `{{ "" }}`}},
		}},
	}}
	r := rules.Rule{Name: "r", Actions: []rules.Action{a}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := CheckTemplates(r.Actions); err != nil {
		t.Fatal(err)
	}
	steps, ok := NewRunner(srv.URL, "test").Run(context.Background(), r.Actions, ev.TemplateData("r"), nil)
	if !ok {
		t.Fatalf("discord action failed: %+v", steps)
	}
	if query != "thread_id=123456789012345678" {
		t.Errorf("query = %q", query)
	}
	if got["content"] != "<@111111111111111111> <@&222222222222222222> Motion" {
		t.Errorf("content = %v", got["content"])
	}
	if got["username"] != ev.EventData["siteName"].(string)+" cams" || got["flags"] != float64(4096) {
		t.Errorf("username/flags = %v / %v", got["username"], got["flags"])
	}
	am := got["allowed_mentions"].(map[string]any)
	if len(am["parse"].([]any)) != 0 || am["users"].([]any)[0] != "111111111111111111" || am["roles"].([]any)[0] != "222222222222222222" {
		t.Errorf("allowed_mentions = %v", am)
	}
	em := got["embeds"].([]any)[0].(map[string]any)
	if em["title"] != "CAM-01" || em["color"] != float64(0xff0000) || em["timestamp"] == nil {
		t.Errorf("embed = %v", em)
	}
	if em["footer"].(map[string]any)["text"] != "YourSix" {
		t.Errorf("footer = %v", em["footer"])
	}
	fields := em["fields"].([]any)
	if fields[0].(map[string]any)["inline"] != true || fields[1].(map[string]any)["value"] != "​" {
		t.Errorf("fields = %v", fields)
	}
}

func TestDiscordValidation(t *testing.T) {
	cases := []struct {
		name string
		a    rules.Action
		want string
	}{
		{"nothing to send", rules.Action{Type: rules.ActionDiscord}, "message or an embed"},
		{"embed only ok", rules.Action{Type: rules.ActionDiscord, Discord: &rules.DiscordOptions{Embeds: []rules.Embed{{Title: "x"}}}}, ""},
		{"empty embed", rules.Action{Type: rules.ActionDiscord, Discord: &rules.DiscordOptions{Embeds: []rules.Embed{{}}}}, "empty"},
		{"bad color", rules.Action{Type: rules.ActionDiscord, Message: "m", Discord: &rules.DiscordOptions{Embeds: []rules.Embed{{Title: "x", Color: "red"}}}}, "color"},
		{"bad user id", rules.Action{Type: rules.ActionDiscord, Message: "m", Discord: &rules.DiscordOptions{PingUsers: []string{"frank"}}}, "Discord ID"},
		{"thread both", rules.Action{Type: rules.ActionDiscord, Message: "m", Discord: &rules.DiscordOptions{ThreadID: "123456789", ThreadName: "x"}}, "not both"},
	}
	for _, c := range cases {
		r := rules.Rule{Name: "r", Actions: []rules.Action{c.a}}
		err := r.Validate()
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestDiscordSecretRefsIncludeEmbeds(t *testing.T) {
	names, err := SecretRefs([]rules.Action{{Type: rules.ActionDiscord, Discord: &rules.DiscordOptions{
		Embeds: []rules.Embed{{Fields: []rules.EmbedField{{Name: "k", Value: `{{ secret "TOKEN" }}`}}}},
	}}})
	if err != nil || len(names) != 1 || names[0] != "TOKEN" {
		t.Fatalf("names = %v, err = %v", names, err)
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

// Watch filters are compared with event data, so they can't use secrets,
// and their templates are checked when the rule is saved.
func TestWatchFilterChecks(t *testing.T) {
	watch := func(v string) []rules.Action {
		return []rules.Action{{Type: rules.ActionDelay, DelayMillis: 5, Watch: &rules.Watch{Stop: true,
			Trigger: rules.Trigger{Filters: []rules.Filter{{Field: "deviceName", Op: rules.OpEquals, Value: v}}}}}}
	}
	if err := ForbiddenSecretRefs(watch(`{{ secret "X" }}`)); err == nil || !strings.Contains(err.Error(), "watch filter 1") {
		t.Fatalf("secret allowed: %v", err)
	}
	if err := ForbiddenSecretRefs(watch(`{{ .data.deviceName }}`)); err != nil {
		t.Fatal(err)
	}
	if err := CheckTemplates(watch(`{{ .data.deviceName`)); err == nil {
		t.Fatal("broken template allowed")
	}
}

// Without an event source (e.g. a runner used on its own), a watched delay
// just waits, then runs its if-no-match actions.
func TestWatchedDelayWithoutEvents(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	acts := []rules.Action{{Type: rules.ActionDelay, DelayMillis: 20, Watch: &rules.Watch{
		Trigger:   rules.Trigger{Codes: []string{"vmd"}},
		IfNoMatch: []rules.Action{{Type: rules.ActionHTTP, Method: "GET", URL: srv.URL}},
	}}}
	res, ok := NewRunner("", "test").Run(context.Background(), acts, map[string]any{}, nil)
	if !ok || len(res) != 1 || res[0].Watch == nil || len(res[0].Watch.Steps) != 1 || hits.Load() != 1 {
		t.Fatalf("ok=%v res=%+v hits=%d", ok, res, hits.Load())
	}
}

// regexquote makes a templated regex filter match a value literally, and a
// regex that's invalid once filled in fails the step instead of silently
// never matching.
func TestWatchRegexFilters(t *testing.T) {
	data := map[string]any{"data": map[string]any{"deviceName": "Cam (1)"}}
	filter := func(v string) rules.Trigger {
		return rules.Trigger{Filters: []rules.Filter{{Field: "deviceName", Op: rules.OpRegex, Value: v}}}
	}
	got, err := renderWatch(filter("^{{ regexquote .data.deviceName }}$"), data)
	if err != nil || got.Filters[0].Value != `^Cam \(1\)$` {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := renderWatch(filter("^{{ .data.deviceName }}[$"), data); err == nil || !strings.Contains(err.Error(), "regexquote") {
		t.Fatalf("invalid regex: %v", err)
	}
}
