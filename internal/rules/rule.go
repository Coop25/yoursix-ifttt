// Package rules defines the programmable "if this event, then these actions"
// model and the logic that decides whether an event matches a rule.
package rules

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"yoursix-ifttt/internal/yoursix"
)

// Rule is one trigger plus an ordered list of actions.
type Rule struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Trigger         Trigger  `json:"trigger"`
	Actions         []Action `json:"actions"`
	CooldownSeconds int      `json:"cooldownSeconds"`
	// CooldownFrom picks when the cooldown window starts: "start" (default,
	// when the last run began) or "finish" (when the last run completed).
	CooldownFrom string `json:"cooldownFrom,omitempty"`
	// SkipIfRunning ignores matching events while a previous run of this
	// rule is still executing. The event is still recorded.
	SkipIfRunning bool `json:"skipIfRunning,omitempty"`
	// UpdatedBy is the Discord ID of the admin who last saved the rule. The
	// rule runs with that admin's secrets. Always set by the server.
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedByName string    `json:"updatedByName,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Trigger selects events. Empty Topics/Codes match anything; every filter must
// pass. Codes may be bare ("vmd") or qualified ("device:vmd").
type Trigger struct {
	Topics  []string `json:"topics"`
	Codes   []string `json:"codes"`
	Filters []Filter `json:"filters"`
}

// Cooldown reference points.
const (
	CooldownFromStart  = "start"
	CooldownFromFinish = "finish"
)

// Filter operators.
const (
	OpEquals     = "equals"
	OpNotEquals  = "not_equals"
	OpContains   = "contains"
	OpRegex      = "regex"
	OpExists     = "exists"
	OpNotExists  = "not_exists"
	OpStartsWith = "starts_with"
)

var validOps = []string{OpEquals, OpNotEquals, OpContains, OpRegex, OpExists, OpNotExists, OpStartsWith}

// Filter compares one event field (see yoursix.Event.Field) with Value.
// String comparisons are case-insensitive except for regex, which can opt in
// with (?i).
type Filter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// Action types.
const (
	ActionHTTP    = "http"
	ActionDelay   = "delay"
	ActionDiscord = "discord"
	// ActionRequest runs a SavedRequest by ID with the given parameters.
	ActionRequest = "request"
)

// Action is a single step. Only the fields relevant to Type are used.
// String fields marked "templated" are rendered with text/template against
// yoursix.Event.TemplateData before running.
type Action struct {
	Type            string `json:"type"`
	Name            string `json:"name,omitempty"`
	ContinueOnError bool   `json:"continueOnError,omitempty"`

	// http
	Method         string            `json:"method,omitempty"`
	URL            string            `json:"url,omitempty"`     // templated
	Headers        map[string]string `json:"headers,omitempty"` // values templated
	Body           string            `json:"body,omitempty"`    // templated
	Auth           string            `json:"auth,omitempty"`    // "", "basic", "digest"
	Username       string            `json:"username,omitempty"`
	Password       string            `json:"password,omitempty"` // templated, so {{ secret "X" }} works
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
	InsecureTLS    bool              `json:"insecureTLS,omitempty"` // local cameras often have self-signed certs
	ExpectStatus   []int             `json:"expectStatus,omitempty"`

	// delay
	DelayMillis int `json:"delayMillis,omitempty"`

	// discord (webhook URL may also come from Y6IFTTT_DISCORD_WEBHOOK_URL)
	WebhookURL string `json:"webhookUrl,omitempty"` // templated
	Message    string `json:"message,omitempty"`    // templated

	// request: a SavedRequest plus values for its parameters. Values are
	// templated with event data but may not use secrets.
	RequestID int64             `json:"requestId,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
}

// Validate normalises the rule in place and reports the first problem.
func (r *Rule) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("name is required")
	}
	if r.CooldownSeconds < 0 {
		return errors.New("cooldownSeconds must be >= 0")
	}
	switch r.CooldownFrom {
	case "", CooldownFromStart:
		r.CooldownFrom = CooldownFromStart
	case CooldownFromFinish:
	default:
		return fmt.Errorf("cooldownFrom must be %q or %q", CooldownFromStart, CooldownFromFinish)
	}
	for i, t := range r.Trigger.Topics {
		r.Trigger.Topics[i] = strings.ToLower(strings.TrimSpace(t))
	}
	for i, c := range r.Trigger.Codes {
		r.Trigger.Codes[i] = strings.ToLower(strings.TrimSpace(c))
	}
	for i := range r.Trigger.Filters {
		f := &r.Trigger.Filters[i]
		f.Field = strings.TrimSpace(f.Field)
		if f.Field == "" {
			return fmt.Errorf("filter %d: field is required", i+1)
		}
		if !slices.Contains(validOps, f.Op) {
			return fmt.Errorf("filter %d: unknown op %q", i+1, f.Op)
		}
		if f.Op == OpRegex {
			if _, err := regexp.Compile(f.Value); err != nil {
				return fmt.Errorf("filter %d: %w", i+1, err)
			}
		}
	}
	if len(r.Actions) == 0 {
		return errors.New("at least one action is required")
	}
	for i := range r.Actions {
		if err := r.Actions[i].validate(); err != nil {
			return fmt.Errorf("action %d: %w", i+1, err)
		}
	}
	return nil
}

func (a *Action) validate() error {
	switch a.Type {
	case ActionHTTP:
		a.Method = strings.ToUpper(strings.TrimSpace(a.Method))
		if a.Method == "" {
			a.Method = http.MethodGet
		}
		if strings.TrimSpace(a.URL) == "" {
			return errors.New("url is required")
		}
		switch a.Auth {
		case "", "basic", "digest":
		default:
			return fmt.Errorf("unknown auth %q", a.Auth)
		}
		if a.TimeoutSeconds < 0 || a.TimeoutSeconds > 120 {
			return errors.New("timeoutSeconds must be between 0 and 120")
		}
	case ActionDelay:
		if a.DelayMillis <= 0 || a.DelayMillis > 10*60*1000 {
			return errors.New("delayMillis must be between 1 and 600000")
		}
	case ActionDiscord:
		if strings.TrimSpace(a.Message) == "" {
			return errors.New("message is required")
		}
	case ActionRequest:
		if a.RequestID <= 0 {
			return errors.New("choose a saved request")
		}
		for k := range a.Params {
			if !validParam.MatchString(k) {
				return fmt.Errorf("invalid parameter name %q", k)
			}
		}
	default:
		return fmt.Errorf("unknown type %q", a.Type)
	}
	return nil
}

// Matches reports whether ev satisfies the trigger.
func (t Trigger) Matches(ev yoursix.Event) bool {
	topic := strings.ToLower(ev.EventTopic)
	code := strings.ToLower(ev.EventCode)
	if len(t.Topics) > 0 && !slices.Contains(t.Topics, topic) {
		return false
	}
	if len(t.Codes) > 0 && !slices.Contains(t.Codes, code) && !slices.Contains(t.Codes, topic+":"+code) {
		return false
	}
	for _, f := range t.Filters {
		if !f.matches(ev) {
			return false
		}
	}
	return true
}

func (f Filter) matches(ev yoursix.Event) bool {
	raw, ok := ev.Field(f.Field)
	switch f.Op {
	case OpExists:
		return ok && raw != nil
	case OpNotExists:
		return !ok || raw == nil
	}
	got := ""
	if ok && raw != nil {
		got = fmt.Sprint(raw)
	}
	switch f.Op {
	case OpEquals:
		return strings.EqualFold(got, f.Value)
	case OpNotEquals:
		return !strings.EqualFold(got, f.Value)
	case OpContains:
		return strings.Contains(strings.ToLower(got), strings.ToLower(f.Value))
	case OpStartsWith:
		return strings.HasPrefix(strings.ToLower(got), strings.ToLower(f.Value))
	case OpRegex:
		re, err := regexp.Compile(f.Value)
		return err == nil && re.MatchString(got)
	}
	return false
}
