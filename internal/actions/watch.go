package actions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/yoursix"
)

// maxWatchSteps limits how many results of a watch's actions are kept in a
// step, so a busy camera with "every match" can't bloat the run history.
const maxWatchSteps = 50

// WatchResult is what a watched delay saw while it waited.
type WatchResult struct {
	Matches  int  `json:"matches"`
	Restarts int  `json:"restarts,omitempty"`
	Stopped  bool `json:"stopped,omitempty"`
	// Steps are the results of the watch's actions, in the order they ran.
	Steps []StepResult `json:"steps,omitempty"`
	// Dropped counts results left out of Steps beyond maxWatchSteps.
	Dropped int `json:"dropped,omitempty"`
}

// WatchFunc subscribes to events matching t until stop is called.
type WatchFunc func(t rules.Trigger) (events <-chan yoursix.Event, stop func())

// noWatchSecrets rejects {{ secret }} in watch filters: their values are
// compared with event data, never sent anywhere, so they've no use for one.
func noWatchSecrets(string) (string, error) {
	return "", errors.New("secrets can't be used in watch filters")
}

// watchedDelay waits like a delay while reacting to matching events as a.Watch
// says. stop means the watch ended the rule; failed means one of its actions
// failed (which doesn't fail the step). data["watch"] is kept up to date for
// the watch's actions and the rule's later steps.
func (r *Runner) watchedDelay(ctx context.Context, i int, a rules.Action, data map[string]any, opts RunOptions, start time.Time, res *StepResult) (stop, failed bool, err error) {
	w := a.Watch
	delay := time.Duration(a.DelayMillis) * time.Millisecond
	until := start.Add(delay)
	limit := until
	if w.Restart {
		limit = start.Add(time.Duration(w.MaxMillis) * time.Millisecond)
	}
	out := &WatchResult{}
	res.Watch = out
	var last yoursix.Event
	publish := func() {
		v := map[string]any{"matches": out.Matches, "restarts": out.Restarts, "stopped": out.Stopped, "waitedMs": time.Since(start).Milliseconds()}
		if out.Matches > 0 {
			td := last.TemplateData("")
			v["event"], v["data"] = td["event"], td["data"]
		}
		data["watch"] = v
	}
	runActs := func(acts []rules.Action, match int) {
		steps, ok := r.RunWith(ctx, acts, data, RunOptions{Secrets: opts.Secrets, Owner: opts.Owner})
		if !ok {
			failed = true
		}
		for _, s := range steps {
			s.Match = match
			if len(out.Steps) < maxWatchSteps {
				out.Steps = append(out.Steps, s)
			} else {
				out.Dropped++
			}
		}
	}

	var events <-chan yoursix.Event
	if opts.Watch != nil {
		// Only the watch's own filters are templates, not the rule's.
		own, err := renderWatch(w.Trigger, data)
		if err != nil {
			return false, false, err
		}
		rendered := *w
		rendered.Trigger = own
		ch, unsubscribe := opts.Watch(rendered.Effective(opts.RuleTrigger))
		defer unsubscribe()
		events = ch
	}

	timer := time.NewTimer(time.Until(until))
	defer timer.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			return false, failed, ctx.Err()
		case <-timer.C:
			break wait
		case ev := <-events:
			out.Matches++
			last = ev
			if w.Restart {
				next := time.Now().Add(delay)
				if next.After(limit) {
					next = limit
				}
				if next.After(until) {
					until = next
					out.Restarts++
					timer.Reset(time.Until(until))
				}
			}
			if w.Stop {
				out.Stopped = true
			}
			publish()
			if opts.OnWatch != nil {
				opts.OnWatch(i, a, *out, until)
			}
			if len(w.Actions) > 0 && (w.EveryMatch || out.Matches == 1) {
				runActs(w.Actions, out.Matches)
				if ctx.Err() != nil {
					return false, failed, ctx.Err()
				}
			}
			if w.Stop {
				return true, failed, nil
			}
		}
	}
	publish()
	if out.Matches == 0 && len(w.IfNoMatch) > 0 {
		runActs(w.IfNoMatch, 0)
	}
	return false, failed, nil
}

// renderWatch fills in the templated filter values of a watch's trigger.
func renderWatch(t rules.Trigger, data map[string]any) (rules.Trigger, error) {
	out := t
	out.Filters = make([]rules.Filter, len(t.Filters))
	for i, f := range t.Filters {
		v, err := Render(f.Value, data, noWatchSecrets)
		if err != nil {
			return out, fmt.Errorf("watch filter %d: %w", i+1, err)
		}
		f.Value = v
		out.Filters[i] = f
	}
	return out, nil
}
