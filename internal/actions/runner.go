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
			if a.Type == rules.ActionDelay {
				until = start.Add(time.Duration(a.DelayMillis) * time.Millisecond)
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
	msg, err := Render(a.Message, data, lk)
	if err != nil {
		return fmt.Errorf("message template: %w", err)
	}
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	payload, _ := json.Marshal(map[string]any{
		"content":          msg,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
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

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// CheckTemplates validates every templated field of a rule's actions.
func CheckTemplates(actions []rules.Action) error {
	for i, a := range actions {
		fields := []string{a.URL, a.Body, a.Password, a.WebhookURL, a.Message}
		for _, v := range a.Headers {
			fields = append(fields, v)
		}
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
		fields := []string{a.URL, a.Body, a.Password, a.WebhookURL, a.Message}
		for _, v := range a.Headers {
			fields = append(fields, v)
		}
		for _, f := range fields {
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

// noSecrets rejects {{ secret }} in saved-request parameters: a rule's
// parameters may be fed into a request someone else wrote (and could point
// anywhere), so they must never carry secrets.
func noSecrets(string) (string, error) {
	return "", errors.New("secrets can't be used in saved-request parameters; put the secret in the saved request itself")
}

func (r *Runner) runSavedRequest(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, owner string, res *StepResult) error {
	if r.Requests == nil {
		return errors.New("saved requests are not available")
	}
	sr, err := r.Requests.SavedRequest(ctx, a.RequestID)
	if err != nil {
		return fmt.Errorf("saved request #%d: %w", a.RequestID, err)
	}
	if res.Name == "" {
		res.Name = sr.Name
	}
	// Re-checked at run time, against the request as it is now.
	refs, err := SecretRefs([]rules.Action{sr.Request})
	if err != nil {
		return fmt.Errorf("saved request %q: %w", sr.Name, err)
	}
	if len(refs) > 0 && sr.UpdatedBy != owner {
		return fmt.Errorf("saved request %q uses secrets belonging to %s; only their rules can use it", sr.Name, nameOr(sr.UpdatedByName, "another admin"))
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
	req := sr.Request
	req.Type = rules.ActionHTTP
	return r.runHTTP(ctx, req, withParams, lk, res)
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
				return fmt.Errorf("action %d, parameter %s: secrets can't be used in parameters; put the secret in the saved request itself", i+1, k)
			}
		}
	}
	return nil
}
