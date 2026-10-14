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
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
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
// secrets resolves {{ secret }} for this run (nil = env secrets only); every
// value it returns is masked in the recorded results.
func (r *Runner) Run(ctx context.Context, actions []rules.Action, data map[string]any, secrets SecretLookup) (results []StepResult, ok bool) {
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
		err := r.runOne(ctx, a, data, lk, &res)
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
		if ctx.Err() != nil {
			stopped = true
		}
	}
	return results, ok
}

func (r *Runner) runOne(ctx context.Context, a rules.Action, data map[string]any, lk SecretLookup, res *StepResult) error {
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
		return err
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
			return err
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
