// Package actions executes rule actions (HTTP calls to cameras and other LAN
// devices, delays, Discord messages).
package actions

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"yoursix-ifttt/internal/rules"
)

const maxResponseSnippet = 2048

// StepResult records the outcome of one action.
type StepResult struct {
	Index      int    `json:"index"`
	Type       string `json:"type"`
	Name       string `json:"name,omitempty"`
	OK         bool   `json:"ok"`
	Skipped    bool   `json:"skipped,omitempty"`
	Status     int    `json:"status,omitempty"`
	Target     string `json:"target,omitempty"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"durationMs"`
	// OnFailure marks steps from the rule's on-failure actions.
	OnFailure bool `json:"onFailure,omitempty"`
}

// Runner executes action lists.
type Runner struct {
	client         *http.Client
	insecureClient *http.Client
	discordDefault string
	userAgent      string

	// Requests resolves "request" actions to saved requests. Nil disables them.
	Requests SavedRequestSource
}

// SavedRequestSource looks up saved requests by ID (the store implements it).
type SavedRequestSource interface {
	SavedRequest(ctx context.Context, id int64) (rules.SavedRequest, error)
}

// RunOptions configures one run.
type RunOptions struct {
	// Secrets resolves {{ secret }} (nil = env secrets only). Every value it
	// returns is masked in the recorded results.
	Secrets SecretLookup
	// Owner is the Discord ID of the rule's owner (who last saved it). Saved
	// requests that use secrets only run for their own owner's rules.
	Owner string
	// OnStep, if set, gets the results so far after each action finishes.
	OnStep func([]StepResult)
	// OnStepStart, if set, is called as each action begins. For a delay, until
	// is when it will end (zero otherwise), so the UI can count down.
	OnStepStart func(index int, a rules.Action, until time.Time)
}

func NewRunner(defaultDiscordWebhook, version string) *Runner {
	insecure := http.DefaultTransport.(*http.Transport).Clone()
	insecure.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in per action for self-signed LAN devices
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{
		client:         &http.Client{CheckRedirect: noRedirect},
		insecureClient: &http.Client{Transport: insecure, CheckRedirect: noRedirect},
		discordDefault: strings.TrimSpace(defaultDiscordWebhook),
		userAgent:      "yoursix-ifttt/" + version,
	}
}

// Run executes actions in order. It stops at the first failure unless that
// action sets ContinueOnError; remaining actions are reported as skipped.
// secrets resolves {{ secret }} for this run (nil = env secrets only).
func (r *Runner) Run(ctx context.Context, actions []rules.Action, data map[string]any, secrets SecretLookup) (results []StepResult, ok bool) {
	return r.RunWith(ctx, actions, data, RunOptions{Secrets: secrets})
}

// RunWith is Run with full options.
func (r *Runner) RunWith(ctx context.Context, actions []rules.Action, data map[string]any, opts RunOptions) (results []StepResult, ok bool) {
	secrets, onStep := opts.Secrets, opts.OnStep
	if secrets == nil {
		secrets = envOnly
	}
	var used []string
	lk := func(name string) (string, error) {
		v, err := secrets(name)
		if err == nil {
			used = append(used, v)
		}
		return v, err
	}
	ok = true
	stopped := false
	for i, a := range actions {
		res := StepResult{Index: i, Type: a.Type, Name: a.Name}
		if stopped {
			res.Skipped = true
			results = append(results, res)
			continue
		}
		start := time.Now()
		if opts.OnStepStart != nil {
			var until time.Time
			if ms := r.delayOf(ctx, a); ms > 0 {
				until = start.Add(time.Duration(ms) * time.Millisecond)
			}
			opts.OnStepStart(i, a, until)
		}
		err := r.runOne(ctx, a, data, lk, opts.Owner, &res)
		res.DurationMs = time.Since(start).Milliseconds()
		if err != nil {
			res.Error = err.Error()
			ok = false
			if !a.ContinueOnError {
				stopped = true
			}
		} else {
			res.OK = true
		}
		redact(&res, used)
		results = append(results, res)
		if onStep != nil {
			onStep(slices.Clone(results))
		}
		if ctx.Err() != nil {
			stopped = true
		}
	}
	return results, ok
}

func (r *Runner) runOne(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, owner string, res *StepResult) error {
	switch a.Type {
	case rules.ActionDelay:
		t := time.NewTimer(time.Duration(a.DelayMillis) * time.Millisecond)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case rules.ActionHTTP:
		return r.runHTTP(ctx, a, data, lk, res)
	case rules.ActionDiscord:
		return r.runDiscord(ctx, a, data, lk, res)
	case rules.ActionRequest:
		return r.runSavedRequest(ctx, a, data, lk, owner, res)
	}
	return fmt.Errorf("unknown action type %q", a.Type)
}

func (r *Runner) runHTTP(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, res *StepResult) error {
	target, err := Render(a.URL, data, lk)
	if err != nil {
		return fmt.Errorf("url template: %w", err)
	}
	res.Target = a.Method + " " + target
	body, err := Render(a.Body, data, lk)
	if err != nil {
		return fmt.Errorf("body template: %w", err)
	}
	headers := make(http.Header, len(a.Headers))
	for k, v := range a.Headers {
		rv, err := Render(v, data, lk)
		if err != nil {
			return fmt.Errorf("header %s template: %w", k, err)
		}
		headers.Set(k, rv)
	}
	password, err := Render(a.Password, data, lk)
	if err != nil {
		return fmt.Errorf("password template: %w", err)
	}

	timeout := time.Duration(a.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := r.client
	if a.InsecureTLS {
		client = r.insecureClient
	}

	build := func() (*http.Request, error) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, a.Method, target, rdr)
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", r.userAgent)
		}
		if body != "" && req.Header.Get("Content-Type") == "" {
			if json.Valid([]byte(body)) {
				req.Header.Set("Content-Type", "application/json")
			} else {
				req.Header.Set("Content-Type", "text/plain; charset=utf-8")
			}
		}
		if a.Auth == "basic" {
			req.SetBasicAuth(a.Username, password)
		}
		return req, nil
	}

	req, err := build()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return explainHTTPError(err, req, timeout)
	}

	if resp.StatusCode == http.StatusUnauthorized && a.Auth == "digest" {
		challenge, found := pickDigestChallenge(resp)
		drain(resp)
		if !found {
			return errors.New("server returned 401 without a Digest challenge")
		}
		req, err = build()
		if err != nil {
			return err
		}
		authz, err := digestAuthorization(challenge, a.Method, req.URL.RequestURI(), a.Username, password)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", authz)
		resp, err = client.Do(req)
		if err != nil {
			return explainHTTPError(err, req, timeout)
		}
	}
	defer drain(resp)

	res.Status = resp.StatusCode
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSnippet))
	res.Response = strings.TrimSpace(string(snippet))

	if len(a.ExpectStatus) > 0 {
		if !slices.Contains(a.ExpectStatus, resp.StatusCode) {
			return fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
	} else if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (r *Runner) runDiscord(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, res *StepResult) error {
	hook := a.WebhookURL
	if strings.TrimSpace(hook) == "" {
		hook = r.discordDefault
	}
	if hook == "" {
		return errors.New("no Discord webhook URL (set it on the action or Y6IFTTT_DISCORD_WEBHOOK_URL)")
	}
	hook, err := Render(hook, data, lk)
	if err != nil {
		return fmt.Errorf("webhook template: %w", err)
	}
	payload, threadID, err := discordPayload(a, data, lk)
	if err != nil {
		return err
	}
	if threadID != "" {
		u, err := url.Parse(hook)
		if err != nil {
			return fmt.Errorf("webhook URL: %w", err)
		}
		q := u.Query()
		q.Set("thread_id", threadID)
		u.RawQuery = q.Encode()
		hook = u.String()
	}
	res.Target = "discord webhook"

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	res.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseSnippet))
		res.Response = string(snippet)
		return fmt.Errorf("discord returned %d", resp.StatusCode)
	}
	return nil
}

// discordFlagSuppressNotifications posts a message without notifying anyone.
const discordFlagSuppressNotifications = 1 << 12

// discordPayload renders a discord action into the webhook's JSON body, and
// returns the thread to post into, if any. Text is cut to Discord's limits.
func discordPayload(a rules.Action, data map[string]any, lk SecretLookup) ([]byte, string, error) {
	var firstErr error
	render := func(what, s string, limit int) string {
		if firstErr != nil || s == "" {
			return ""
		}
		out, err := Render(s, data, lk)
		if err != nil {
			firstErr = fmt.Errorf("%s template: %w", what, err)
			return ""
		}
		return truncate(strings.TrimSpace(out), limit)
	}

	d := a.Discord
	if d == nil {
		d = &rules.DiscordOptions{}
	}
	content := render("message", a.Message, 2000)
	var prefix []string
	for _, id := range d.PingUsers {
		if !strings.Contains(content, "<@"+id+">") && !strings.Contains(content, "<@!"+id+">") {
			prefix = append(prefix, "<@"+id+">")
		}
	}
	for _, id := range d.PingRoles {
		if !strings.Contains(content, "<@&"+id+">") {
			prefix = append(prefix, "<@&"+id+">")
		}
	}
	if len(prefix) > 0 {
		content = truncate(strings.TrimSpace(strings.Join(prefix, " ")+" "+content), 2000)
	}

	mentions := map[string]any{"parse": []string{}}
	if d.PingEveryone {
		mentions["parse"] = []string{"everyone"}
	}
	if len(d.PingUsers) > 0 {
		mentions["users"] = d.PingUsers
	}
	if len(d.PingRoles) > 0 {
		mentions["roles"] = d.PingRoles
	}
	body := map[string]any{"allowed_mentions": mentions}
	if content != "" {
		body["content"] = content
	}
	if v := render("username", d.Username, 80); v != "" {
		body["username"] = v
	}
	if v := render("avatar URL", d.AvatarURL, 2048); v != "" {
		body["avatar_url"] = v
	}
	if v := render("thread name", d.ThreadName, 100); v != "" {
		body["thread_name"] = v
	}
	if d.TTS {
		body["tts"] = true
	}
	if d.Silent {
		body["flags"] = discordFlagSuppressNotifications
	}

	var embeds []map[string]any
	for i, e := range d.Embeds {
		what := func(f string) string { return fmt.Sprintf("embed %d %s", i+1, f) }
		em := map[string]any{}
		set := func(m map[string]any, k, v string) {
			if v != "" {
				m[k] = v
			}
		}
		set(em, "title", render(what("title"), e.Title, 256))
		set(em, "url", render(what("URL"), e.URL, 2048))
		set(em, "description", render(what("description"), e.Description, 4096))
		if e.Color != "" {
			if n, err := strconv.ParseUint(strings.TrimPrefix(e.Color, "#"), 16, 32); err == nil {
				em["color"] = n
			}
		}
		if ts := render(what("timestamp"), e.Timestamp, 64); ts != "" {
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("embed %d timestamp %q isn't an RFC 3339 time", i+1, ts)
			}
			em["timestamp"] = t.UTC().Format(time.RFC3339)
		}
		author := map[string]any{}
		set(author, "name", render(what("author name"), e.AuthorName, 256))
		set(author, "url", render(what("author URL"), e.AuthorURL, 2048))
		set(author, "icon_url", render(what("author icon"), e.AuthorIconURL, 2048))
		if author["name"] != nil {
			em["author"] = author
		}
		footer := map[string]any{}
		set(footer, "text", render(what("footer"), e.FooterText, 2048))
		set(footer, "icon_url", render(what("footer icon"), e.FooterIconURL, 2048))
		if footer["text"] != nil {
			em["footer"] = footer
		}
		if v := render(what("thumbnail"), e.ThumbnailURL, 2048); v != "" {
			em["thumbnail"] = map[string]any{"url": v}
		}
		if v := render(what("image"), e.ImageURL, 2048); v != "" {
			em["image"] = map[string]any{"url": v}
		}
		var fields []map[string]any
		for j, f := range e.Fields {
			fw := func(p string) string { return what(fmt.Sprintf("field %d %s", j+1, p)) }
			fields = append(fields, map[string]any{
				// Discord rejects empty names and values, e.g. from missing event data.
				"name":   orBlank(render(fw("name"), f.Name, 256)),
				"value":  orBlank(render(fw("value"), f.Value, 1024)),
				"inline": f.Inline,
			})
		}
		if fields != nil {
			em["fields"] = fields
		}
		embeds = append(embeds, em)
	}
	if embeds != nil {
		body["embeds"] = embeds
	}
	if firstErr != nil {
		return nil, "", firstErr
	}
	if content == "" && embeds == nil {
		return nil, "", errors.New("the message rendered empty and there are no embeds")
	}
	payload, err := json.Marshal(body)
	return payload, d.ThreadID, err
}

// truncate cuts s to at most n characters (runes).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func orBlank(s string) string {
	if s == "" {
		return "​"
	}
	return s
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// CheckTemplates validates every templated field of a rule's actions.
func CheckTemplates(actions []rules.Action) error {
	for i, a := range actions {
		fields := a.Templates()
		for _, v := range a.Params {
			fields = append(fields, v)
		}
		for _, f := range fields {
			if err := CheckTemplate(f); err != nil {
				return fmt.Errorf("action %d: %w", i+1, err)
			}
		}
	}
	return nil
}

// redact masks secret values that may have ended up in a step's recorded
// URL, response, or error. Very short values are skipped to avoid mangling
// unrelated text; give real secrets some length.
func redact(res *StepResult, values []string) {
	for _, v := range values {
		if len(v) < 4 {
			continue
		}
		res.Target = strings.ReplaceAll(res.Target, v, "••••••")
		res.Response = strings.ReplaceAll(res.Response, v, "••••••")
		res.Error = strings.ReplaceAll(res.Error, v, "••••••")
		if u := url.QueryEscape(v); u != v {
			res.Target = strings.ReplaceAll(res.Target, u, "••••••")
		}
	}
}

// SecretRefs returns every secret name referenced by the actions' templates.
func SecretRefs(actions []rules.Action) ([]string, error) {
	seen := map[string]struct{}{}
	for i, a := range actions {
		for _, f := range a.Templates() {
			names, err := SecretNames(f)
			if err != nil {
				return nil, fmt.Errorf("action %d: %w", i+1, err)
			}
			for _, n := range names {
				seen[n] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// explainHTTPError turns the usual network failures into messages that say
// what to check. Rules typically call devices on the LAN, so the common
// cause is that the app's container can't reach the device.
func explainHTTPError(err error, req *http.Request, timeout time.Duration) error {
	host := req.URL.Host
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return fmt.Errorf("no response from %s within %s: check it's reachable from the app's container (or raise the action's timeout)", host, timeout)
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled: the app was shutting down")
	case errors.As(err, &dnsErr):
		return fmt.Errorf("can't resolve %q from the app's container: use an IP address or a name its DNS knows", req.URL.Hostname())
	case isErrno(err, syscall.ECONNREFUSED, wsaECONNREFUSED):
		return fmt.Errorf("%s refused the connection: wrong port, or the service isn't running", host)
	case isErrno(err, syscall.EHOSTUNREACH, syscall.ENETUNREACH, wsaEHOSTUNREACH, wsaENETUNREACH):
		return fmt.Errorf("%s is unreachable from the app's container: check its network routing", host)
	}
	return err
}

// Winsock reports these conditions with its own codes, so match both when the
// app (or its tests) runs on Windows.
const (
	wsaENETUNREACH  syscall.Errno = 10051
	wsaECONNREFUSED syscall.Errno = 10061
	wsaEHOSTUNREACH syscall.Errno = 10065
)

func isErrno(err error, codes ...syscall.Errno) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return slices.Contains(codes, errno)
}

// delayOf is how long a step waits, if it's a delay or a saved delay, so
// progress can show a countdown.
func (r *Runner) delayOf(ctx context.Context, a rules.Action) int {
	switch a.Type {
	case rules.ActionDelay:
		return a.DelayMillis
	case rules.ActionRequest:
		if r.Requests != nil {
			if sr, err := r.Requests.SavedRequest(ctx, a.RequestID); err == nil && sr.Request.Type == rules.ActionDelay {
				return sr.Request.DelayMillis
			}
		}
	}
	return 0
}

// noSecrets rejects {{ secret }} in saved-action parameters: a rule's
// parameters may be fed into an action someone else wrote (and could point
// anywhere), so they must never carry secrets.
func noSecrets(string) (string, error) {
	return "", errors.New("secrets can't be used in saved-action parameters; put the secret in the saved action itself")
}

// runSavedRequest runs a saved action (an HTTP request, delay or Discord
// message) with the rule's parameter values.
func (r *Runner) runSavedRequest(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, owner string, res *StepResult) error {
	if r.Requests == nil {
		return errors.New("saved actions are not available")
	}
	sr, err := r.Requests.SavedRequest(ctx, a.RequestID)
	if err != nil {
		return fmt.Errorf("saved action #%d: %w", a.RequestID, err)
	}
	if res.Name == "" {
		res.Name = sr.Name
	}
	// Re-checked at run time, against the action as it is now.
	refs, err := SecretRefs([]rules.Action{sr.Request})
	if err != nil {
		return fmt.Errorf("saved action %q: %w", sr.Name, err)
	}
	if len(refs) > 0 && sr.UpdatedBy != owner {
		return fmt.Errorf("saved action %q uses secrets belonging to %s; only their rules can use it", sr.Name, nameOr(sr.UpdatedByName, "another admin"))
	}

	given := make(map[string]string, len(a.Params))
	for k, v := range a.Params {
		rv, err := Render(v, data, noSecrets)
		if err != nil {
			return fmt.Errorf("parameter %s: %w", k, err)
		}
		given[k] = rv
	}
	params, err := sr.ResolveParams(given)
	if err != nil {
		return err
	}

	withParams := make(map[string]any, len(data)+1)
	for k, v := range data {
		withParams[k] = v
	}
	withParams["params"] = params
	switch sr.Request.Type {
	case "", rules.ActionHTTP:
		req := sr.Request
		req.Type = rules.ActionHTTP
		return r.runHTTP(ctx, req, withParams, lk, res)
	case rules.ActionDelay, rules.ActionDiscord:
		return r.runOne(ctx, sr.Request, withParams, lk, owner, res)
	}
	return fmt.Errorf("saved action %q can't run actions of type %q", sr.Name, sr.Request.Type)
}

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// ParamSecretRefs rejects saved-request parameters that use secrets, so
// rules can be checked at save time as well as at run time.
func ParamSecretRefs(actions []rules.Action) error {
	for i, a := range actions {
		if a.Type != rules.ActionRequest {
			continue
		}
		for k, v := range a.Params {
			names, err := SecretNames(v)
			if err != nil {
				return fmt.Errorf("action %d, parameter %s: %w", i+1, k, err)
			}
			if len(names) > 0 {
				return fmt.Errorf("action %d, parameter %s: secrets can't be used in parameters; put the secret in the saved action itself", i+1, k)
			}
		}
	}
	return nil
}
