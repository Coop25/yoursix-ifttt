package rules

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxWatchMillis caps how long a watched delay can keep waiting when matches
// restart its countdown. A run holds one of the engine's run slots while it
// waits, so it can't be unbounded.
const MaxWatchMillis = 60 * 60 * 1000

// Watch makes a delay step react to events that arrive while it waits: it
// can restart the countdown (keep a light on while there's motion), run
// actions, or stop the rule. Events it sees are still matched against every
// rule as usual.
type Watch struct {
	// SameAsTrigger matches events against the rule's own trigger, plus
	// Trigger.Filters. Trigger's topics and codes are ignored.
	SameAsTrigger bool `json:"sameAsTrigger,omitempty"`
	// Trigger selects the events to react to. Filter values are templated
	// with the run's data when the delay starts, so
	// {{ .data.deviceName }} means "the camera that started this run".
	Trigger Trigger `json:"trigger"`

	// Restart starts the countdown again on each match, up to MaxMillis
	// after the delay began.
	Restart   bool `json:"restart,omitempty"`
	MaxMillis int  `json:"maxMillis,omitempty"`
	// Actions run on the first match, or on every match with EveryMatch.
	// They behave as if set to continue on error: a failure is recorded and
	// the run counts as failed, but the wait carries on.
	Actions    []Action `json:"actions,omitempty"`
	EveryMatch bool     `json:"everyMatch,omitempty"`
	// Stop ends the wait on a match and skips the rule's remaining steps
	// (after running Actions). The run still counts as successful.
	Stop bool `json:"stop,omitempty"`
	// IfNoMatch runs when the wait ends without any match.
	IfNoMatch []Action `json:"ifNoMatch,omitempty"`
}

// Templates returns the watch's templated filter values. They're rendered
// without secrets.
func (w Watch) Templates() []string {
	out := make([]string, 0, len(w.Trigger.Filters))
	for _, f := range w.Trigger.Filters {
		out = append(out, f.Value)
	}
	return out
}

// Effective is the trigger the watch matches with, given the rule's own.
func (w Watch) Effective(rule Trigger) Trigger {
	if !w.SameAsTrigger {
		return w.Trigger
	}
	return Trigger{
		Topics:  rule.Topics,
		Codes:   rule.Codes,
		Filters: append(append([]Filter{}, rule.Filters...), w.Trigger.Filters...),
	}
}

func (w *Watch) validate(delayMillis int) error {
	if err := w.Trigger.validate(true); err != nil {
		return err
	}
	if w.SameAsTrigger {
		w.Trigger.Topics, w.Trigger.Codes = []string{}, []string{}
	}
	for _, list := range []*[]string{&w.Trigger.Topics, &w.Trigger.Codes} {
		if *list == nil {
			*list = []string{}
		}
	}
	if w.Trigger.Filters == nil {
		w.Trigger.Filters = []Filter{}
	}
	if !w.SameAsTrigger && len(w.Trigger.Topics) == 0 && len(w.Trigger.Codes) == 0 && len(w.Trigger.Filters) == 0 {
		return errors.New("watch: pick the events to watch for, or use the rule's trigger")
	}
	if !w.Restart && !w.Stop && len(w.Actions) == 0 && len(w.IfNoMatch) == 0 {
		return errors.New("watch: choose what happens (restart the countdown, run actions, stop the rule, or run actions if nothing matches)")
	}
	if w.Restart {
		if w.MaxMillis < delayMillis || w.MaxMillis > MaxWatchMillis {
			return fmt.Errorf("watch: the longest wait must be at least the delay (%s s) and at most 1 hour (%d s)",
				strconv.FormatFloat(float64(delayMillis)/1000, 'f', -1, 64), MaxWatchMillis/1000)
		}
	} else {
		w.MaxMillis = 0
	}
	if len(w.Actions) == 0 {
		w.EveryMatch = false
	}
	for _, list := range []struct {
		name string
		acts []Action
	}{{"on-match action", w.Actions}, {"if-no-match action", w.IfNoMatch}} {
		for i := range list.acts {
			a := &list.acts[i]
			if a.Watch != nil {
				return fmt.Errorf("watch: %s %d: actions that run while waiting can't watch for events themselves", list.name, i+1)
			}
			if err := a.validate(); err != nil {
				return fmt.Errorf("watch: %s %d: %w", list.name, i+1, err)
			}
		}
	}
	return nil
}

// isTemplated reports whether a filter value is a template, which can only
// be checked once rendered.
func isTemplated(s string) bool { return strings.Contains(s, "{{") }
