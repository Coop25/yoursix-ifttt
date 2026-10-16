// Package engine connects received events to rules and runs their actions.
package engine

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

// Rule runs are bounded so a burst of motion events cannot open hundreds of
// concurrent connections to a camera.
const maxConcurrentRuns = 16

type Engine struct {
	store   *store.Store
	runner  *actions.Runner
	secrets *secrets.Manager
	log     *slog.Logger

	sem chan struct{}
	wg  sync.WaitGroup

	// ctx outlives individual webhook requests; cancelled on shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	// OnChange, if set, is told whenever an event or one of its runs changes
	// (received, run started, step finished, run finished), so the UI can be
	// updated live. Set it before the engine handles events.
	OnChange func(eventRow int64)

	// mu guards the per-rule scheduling state below.
	mu           sync.Mutex
	lastFired    map[int64]time.Time // when the last run started
	lastFinished map[int64]time.Time // when the last run finished
	running      map[int64]int       // in-flight runs per rule
}

func New(s *store.Store, r *actions.Runner, sm *secrets.Manager, log *slog.Logger) *Engine {
	// Saved requests ("request" actions) come from the same store as the rules.
	if r.Requests == nil {
		r.Requests = s
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		store:        s,
		secrets:      sm,
		runner:       r,
		log:          log,
		sem:          make(chan struct{}, maxConcurrentRuns),
		ctx:          ctx,
		cancel:       cancel,
		lastFired:    map[int64]time.Time{},
		lastFinished: map[int64]time.Time{},
		running:      map[int64]int{},
	}
	// Seed cooldowns so a restart doesn't immediately re-fire.
	if all, err := s.ListRules(ctx); err == nil {
		for _, r := range all {
			if r.LastFiredAt != nil {
				e.lastFired[r.ID] = *r.LastFiredAt
			}
			if r.LastFinishedAt != nil {
				e.lastFinished[r.ID] = *r.LastFinishedAt
			}
		}
	}
	return e
}

// Result summarises what Dispatch did with an event.
type Result struct {
	EventRow  int64   `json:"eventRow"`
	Duplicate bool    `json:"duplicate"`
	Matched   []int64 `json:"matchedRules"`
}

// Dispatch records ev and starts every matching enabled rule asynchronously.
func (e *Engine) Dispatch(ctx context.Context, ev yoursix.Event, raw []byte, sourceIP string, test bool) (Result, error) {
	row, dup, err := e.store.InsertEvent(ctx, ev, raw, sourceIP, test)
	if err != nil {
		return Result{}, err
	}
	res := Result{EventRow: row, Duplicate: dup, Matched: []int64{}}
	if dup {
		e.log.Info("duplicate event ignored", "eventId", ev.EventID, "event", ev.Key())
		return res, nil
	}

	enabled, err := e.store.EnabledRules(ctx)
	if err != nil {
		return res, err
	}
	for _, r := range enabled {
		if !r.Trigger.Matches(ev) {
			continue
		}
		res.Matched = append(res.Matched, r.ID)
		skip := ""
		if !e.inHours(ctx, r) {
			skip = store.RunOffHours
		} else {
			skip = e.claim(r)
		}
		if skip != "" {
			if _, err := e.store.StartRun(ctx, row, r, skip); err != nil {
				e.log.Error("record skipped run", "rule", r.Name, "err", err)
			}
			e.log.Info("rule skipped", "rule", r.Name, "reason", skip, "event", ev.Key())
			continue
		}
		e.startRun(row, r, ev)
	}
	e.log.Info("event received", "event", ev.Key(), "eventId", ev.EventID, "matched", len(res.Matched), "test", test)
	e.changed(row)
	return res, nil
}

// StartTestRun runs one rule against ev in the background, ignoring
// enabled/trigger/cooldown (the UI's "Test" button). It returns as soon as
// the run is recorded; poll the store for its progress. The run uses the
// engine's context, not the request's, so a proxy timing out the HTTP
// request (e.g. Cloudflare's 100s limit) can't cancel it midway.
func (e *Engine) StartTestRun(ctx context.Context, r rules.Rule, ev yoursix.Event, raw []byte, sourceIP string) (store.RunView, error) {
	row, _, err := e.store.InsertEvent(ctx, ev, raw, sourceIP, true)
	if err != nil {
		return store.RunView{}, err
	}
	runID, err := e.store.StartRun(ctx, row, r, store.RunRunning)
	if err != nil {
		return store.RunView{}, err
	}
	e.changed(row)

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.execute(row, runID, r, ev)
	}()
	return store.RunView{ID: runID, EventRow: row, RuleID: r.ID, RuleName: r.Name, Status: store.RunRunning, Steps: []actions.StepResult{}}, nil
}

// stepStarted records which step a run is on (and, for a delay, when it
// ends), so the UI can show live progress and a countdown.
func (e *Engine) stepStarted(row, runID int64) func(int, rules.Action, time.Time) {
	return func(i int, a rules.Action, until time.Time) {
		cur := store.StepInProgress{Index: i, Type: a.Type, Name: e.stepLabel(a), StartedAt: time.Now()}
		if !until.IsZero() {
			cur.Until = &until
		}
		if err := e.store.UpdateRunCurrent(context.Background(), runID, cur); err != nil {
			e.log.Error("save current step", "run", runID, "err", err)
		}
		e.changed(row)
	}
}

func (e *Engine) stepLabel(a rules.Action) string {
	if a.Name != "" {
		return a.Name
	}
	switch a.Type {
	case rules.ActionRequest:
		if sr, err := e.store.SavedRequest(context.Background(), a.RequestID); err == nil {
			return sr.Name
		}
		return "Saved action"
	case rules.ActionDelay:
		return "Delay"
	case rules.ActionDiscord:
		return "Discord message"
	}
	return "HTTP request"
}

// progress saves a run's completed steps as it goes, so the UI can show
// long runs live.
func (e *Engine) progress(row, runID int64) func([]actions.StepResult) {
	return func(steps []actions.StepResult) {
		if err := e.store.UpdateRunSteps(context.Background(), runID, steps); err != nil {
			e.log.Error("save run progress", "run", runID, "err", err)
		}
		e.changed(row)
	}
}

func (e *Engine) finish(row, runID int64, r rules.Rule, ev yoursix.Event, steps []actions.StepResult, ok bool) {
	status := store.RunOK
	if !ok {
		status = store.RunFailed
	}
	if err := e.store.FinishRun(context.Background(), runID, status, steps); err != nil {
		e.log.Error("finish run", "rule", r.Name, "err", err)
	}
	e.log.Info("rule ran", "rule", r.Name, "status", status, "event", ev.Key())
	e.changed(row)
}

// claim decides whether r may run now. It returns "" and reserves the run
// (marking it in flight and starting its cooldown) when allowed, or the
// status to record when the event must be accepted but not acted on.
// Check and reserve happen under one lock so two events arriving together
// cannot both slip through.
func (e *Engine) claim(r rules.Rule) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()

	if r.SkipIfRunning && e.running[r.ID] > 0 {
		return store.RunBusy
	}
	if r.CooldownSeconds > 0 {
		window := time.Duration(r.CooldownSeconds) * time.Second
		if r.CooldownFrom == rules.CooldownFromFinish {
			// The window only opens once the previous run is done.
			if e.running[r.ID] > 0 {
				return store.RunCooldown
			}
			if last, ok := e.lastFinished[r.ID]; ok && now.Sub(last) < window {
				return store.RunCooldown
			}
		} else if last, ok := e.lastFired[r.ID]; ok && now.Sub(last) < window {
			return store.RunCooldown
		}
	}

	e.lastFired[r.ID] = now
	e.running[r.ID]++
	return ""
}

// release marks one run of ruleID finished.
func (e *Engine) release(ruleID int64) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if e.running[ruleID]--; e.running[ruleID] <= 0 {
		delete(e.running, ruleID)
	}
	e.lastFinished[ruleID] = now
	return now
}

// Running reports how many runs of ruleID are in flight.
func (e *Engine) Running(ruleID int64) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[ruleID]
}

func (e *Engine) startRun(row int64, r rules.Rule, ev yoursix.Event) {
	runID, err := e.store.StartRun(e.ctx, row, r, store.RunRunning)
	if err != nil {
		e.log.Error("start run", "rule", r.Name, "err", err)
		e.release(r.ID)
		return
	}
	_ = e.store.MarkRuleFired(e.ctx, r.ID, time.Now())

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			done := e.release(r.ID)
			_ = e.store.MarkRuleFinished(context.Background(), r.ID, done)
		}()
		select {
		case e.sem <- struct{}{}:
		case <-e.ctx.Done():
			_ = e.store.FinishRun(context.Background(), runID, store.RunFailed, nil)
			e.changed(row)
			return
		}
		defer func() { <-e.sem }()
		e.execute(row, runID, r, ev)
	}()
}

// execute runs a rule's actions, then its on-failure actions if any step
// failed, recording progress as it goes. On-failure steps are appended to
// the run's steps, marked OnFailure; the run still counts as failed.
func (e *Engine) execute(row, runID int64, r rules.Rule, ev yoursix.Event) {
	data := ev.TemplateData(r.Name)
	opts := actions.RunOptions{
		Secrets:     e.secrets.LookupFor(e.ctx, r.UpdatedBy),
		Owner:       r.UpdatedBy,
		OnStep:      e.progress(row, runID),
		OnStepStart: e.stepStarted(row, runID),
	}
	steps, ok := e.runner.RunWith(e.ctx, r.Actions, data, opts)
	if !ok && len(r.OnFailure) > 0 && e.ctx.Err() == nil {
		data["failure"] = FailureData(steps)
		main := steps
		mark := func(extra []actions.StepResult) []actions.StepResult {
			out := append(slices.Clip(main), extra...)
			for i := len(main); i < len(out); i++ {
				out[i].Index += len(main)
				out[i].OnFailure = true
			}
			return out
		}
		save, started := opts.OnStep, opts.OnStepStart
		opts.OnStep = func(s []actions.StepResult) { save(mark(s)) }
		opts.OnStepStart = func(i int, a rules.Action, until time.Time) { started(i+len(main), a, until) }
		extra, _ := e.runner.RunWith(e.ctx, r.OnFailure, data, opts)
		steps = mark(extra)
	}
	e.finish(row, runID, r, ev, steps, ok)
}

// FailureData is what on-failure actions see as .failure: the first failed
// step's name, 1-based number and error.
func FailureData(steps []actions.StepResult) map[string]any {
	for _, s := range steps {
		if !s.OK && !s.Skipped && s.Error != "" {
			name := s.Name
			if name == "" {
				name = s.Type
			}
			return map[string]any{"step": name, "stepNumber": s.Index + 1, "error": s.Error}
		}
	}
	return map[string]any{"step": "", "stepNumber": 0, "error": ""}
}

func (e *Engine) changed(row int64) {
	if e.OnChange != nil && row > 0 {
		e.OnChange(row)
	}
}

// Shutdown cancels in-flight runs and waits for them, up to ctx's deadline.
func (e *Engine) Shutdown(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		// Give in-flight runs a chance to finish before cancelling them.
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		e.cancel()
		<-done
	}
	e.cancel()
}

// Forget drops cooldown state for a deleted rule.
func (e *Engine) Forget(ruleID int64) {
	e.mu.Lock()
	delete(e.lastFired, ruleID)
	delete(e.lastFinished, ruleID)
	e.mu.Unlock()
}

// Preview renders every templated value of r's actions (and on-failure
// actions) against ev, without running anything. Secrets show as
// placeholders. On-failure actions see a sample .failure for step 1.
func (e *Engine) Preview(ctx context.Context, r rules.Rule, ev yoursix.Event) (acts, onFailure [][]actions.PreviewField) {
	data := ev.TemplateData(r.Name)
	for _, a := range r.Actions {
		acts = append(acts, e.runner.Preview(ctx, a, data))
	}
	if len(r.OnFailure) > 0 {
		first := "Action 1"
		if len(r.Actions) > 0 && r.Actions[0].Name != "" {
			first = r.Actions[0].Name
		}
		data["failure"] = map[string]any{"step": first, "stepNumber": 1, "error": "sample error: connection refused"}
		for _, a := range r.OnFailure {
			onFailure = append(onFailure, e.runner.Preview(ctx, a, data))
		}
	}
	return acts, onFailure
}

// inHours reports whether r may run now under its schedule, if it has one.
// A schedule that can't be read (it can't be deleted while in use, so this
// means a database problem) doesn't block the rule.
func (e *Engine) inHours(ctx context.Context, r rules.Rule) bool {
	if r.ScheduleID == 0 {
		return true
	}
	sc, err := e.store.Schedule(ctx, r.ScheduleID)
	if err != nil {
		e.log.Error("load schedule", "rule", r.Name, "schedule", r.ScheduleID, "err", err)
		return true
	}
	return sc.Active(time.Now())
}
