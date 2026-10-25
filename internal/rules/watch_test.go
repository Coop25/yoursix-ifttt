package rules

import (
	"errors"
	"strings"
	"testing"

	"yoursix-ifttt/internal/yoursix"
)

func TestWatchValidation(t *testing.T) {
	delay := func(w *Watch) Action { return Action{Type: ActionDelay, DelayMillis: 1000, Watch: w} }
	notify := []Action{{Type: ActionDiscord, Message: "hi"}}
	codes := Trigger{Codes: []string{"aoa"}}

	cases := []struct {
		name string
		r    Rule
		err  string // "" = valid
	}{
		{"restart with same trigger", Rule{Actions: []Action{delay(&Watch{SameAsTrigger: true, Restart: true, MaxMillis: 60000})}}, ""},
		{"actions", Rule{Actions: []Action{delay(&Watch{Trigger: codes, Actions: notify})}}, ""},
		{"stop", Rule{Actions: []Action{delay(&Watch{Trigger: codes, Stop: true})}}, ""},
		{"if no match", Rule{Actions: []Action{delay(&Watch{Trigger: codes, IfNoMatch: notify})}}, ""},
		{"templated regex", Rule{Actions: []Action{delay(&Watch{Stop: true,
			Trigger: Trigger{Filters: []Filter{{Field: "deviceName", Op: OpRegex, Value: "^{{ .data.deviceName }}$"}}}})}}, ""},
		{"bad regex", Rule{Actions: []Action{delay(&Watch{Stop: true,
			Trigger: Trigger{Filters: []Filter{{Field: "deviceName", Op: OpRegex, Value: "("}}}})}}, "filter 1"},
		{"no events chosen", Rule{Actions: []Action{delay(&Watch{Stop: true})}}, "pick the events"},
		{"nothing to do", Rule{Actions: []Action{delay(&Watch{Trigger: codes})}}, "choose what happens"},
		{"restart without limit", Rule{Actions: []Action{delay(&Watch{Trigger: codes, Restart: true})}}, "longest wait"},
		{"limit under delay", Rule{Actions: []Action{delay(&Watch{Trigger: codes, Restart: true, MaxMillis: 500})}}, "at least the delay (1 s)"},
		{"limit over an hour", Rule{Actions: []Action{delay(&Watch{Trigger: codes, Restart: true, MaxMillis: MaxWatchMillis + 1})}}, "longest wait"},
		{"not a delay", Rule{Actions: []Action{{Type: ActionDiscord, Message: "x", Watch: &Watch{Trigger: codes, Stop: true}}}}, "only delays"},
		{"on failure", Rule{Actions: notify, OnFailure: []Action{delay(&Watch{Trigger: codes, Stop: true})}}, "on-failure action 1"},
		{"nested watch", Rule{Actions: []Action{delay(&Watch{Trigger: codes,
			Actions: []Action{delay(&Watch{Trigger: codes, Stop: true})}})}}, "can't watch for events themselves"},
		{"bad watch action", Rule{Actions: []Action{delay(&Watch{Trigger: codes, IfNoMatch: []Action{{Type: ActionHTTP}}})}}, "if-no-match action 1: url is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.r.Name = "x"
			err := c.r.Validate()
			if c.err == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Fatalf("error = %v, want %q", err, c.err)
			}
		})
	}
}

// Normalising drops settings that don't apply.
func TestWatchNormalises(t *testing.T) {
	r := Rule{Name: "x", Actions: []Action{{Type: ActionDelay, DelayMillis: 1000, Watch: &Watch{
		SameAsTrigger: true, Trigger: Trigger{Topics: []string{"device"}, Codes: []string{"vmd"}},
		Stop: true, MaxMillis: 9000, EveryMatch: true,
	}}}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	w := r.Actions[0].Watch
	if len(w.Trigger.Topics) != 0 || len(w.Trigger.Codes) != 0 || w.MaxMillis != 0 || w.EveryMatch {
		t.Fatalf("watch: %+v", w)
	}
}

func TestWatchEffectiveTrigger(t *testing.T) {
	rule := Trigger{Codes: []string{"vmd"}, Filters: []Filter{{Field: "siteName", Op: OpEquals, Value: "HQ"}}}
	own := Filter{Field: "deviceName", Op: OpEquals, Value: "CAM-02"}
	w := Watch{SameAsTrigger: true, Trigger: Trigger{Filters: []Filter{own}}}
	eff := w.Effective(rule)
	if len(eff.Filters) != 2 || len(rule.Filters) != 1 {
		t.Fatalf("effective %+v, rule %+v", eff, rule)
	}
	if eff.Matches(yoursix.Sample("device", "vmd")) { // CAM-01
		t.Fatal("matched another camera")
	}
	w.SameAsTrigger = false
	if got := w.Effective(rule); len(got.Codes) != 0 || len(got.Filters) != 1 {
		t.Fatalf("own trigger: %+v", got)
	}
}

func TestAllActionsIncludesWatchActions(t *testing.T) {
	r := Rule{
		Actions: []Action{{Type: ActionDelay, Watch: &Watch{
			Actions:   []Action{{Type: ActionRequest, RequestID: 1}},
			IfNoMatch: []Action{{Type: ActionRequest, RequestID: 2}},
		}}},
		OnFailure: []Action{{Type: ActionDiscord}},
	}
	all := r.AllActions()
	if len(all) != 4 || all[2].RequestID != 1 || all[3].RequestID != 2 {
		t.Fatalf("all: %+v", all)
	}
}

func TestSavedDelayCantWatch(t *testing.T) {
	sr := SavedRequest{Name: "x", Request: Action{Type: ActionDelay, DelayMillis: 5, Watch: &Watch{Trigger: Trigger{Codes: []string{"vmd"}}, Stop: true}}}
	if err := sr.Validate(); err == nil || !strings.Contains(err.Error(), "saved delay") {
		t.Fatalf("err = %v", err)
	}
}

// Errors about an entry of AllActions name where it is in the rule.
func TestRelabel(t *testing.T) {
	r := Rule{
		Actions: []Action{{Type: ActionDiscord}, {Type: ActionDelay, Watch: &Watch{
			Actions:   []Action{{Type: ActionDiscord}},
			IfNoMatch: []Action{{Type: ActionDiscord}, {Type: ActionDiscord}},
		}}},
		OnFailure: []Action{{Type: ActionDiscord}},
	}
	labels := r.ActionLabels()
	if len(labels) != len(r.AllActions()) {
		t.Fatalf("%d labels for %d actions", len(labels), len(r.AllActions()))
	}
	cases := map[int]string{
		1: "action 2: boom",
		2: "on-failure action 1: boom",
		3: "action 2, watch action 1: boom",
		5: "action 2, if-no-match action 2: boom",
	}
	for i, want := range cases {
		if got := r.Relabel(&ActionError{Index: i, Err: errors.New("boom")}).Error(); got != want {
			t.Errorf("index %d: %q, want %q", i, got, want)
		}
	}
	if got := r.Relabel(errors.New("other")).Error(); got != "other" {
		t.Errorf("plain error: %q", got)
	}
}
