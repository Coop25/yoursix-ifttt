// Package engine connects received events to rules and runs their actions.
package engine

import (
	"context"
	"log/slog"
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

	// mu guards the per-rule scheduling state below.
	mu           sync.Mutex
	lastFired    map[int64]time.Time // when the last run started
	lastFinished map[int64]time.Time // when the last run finished
	running      map[int64]int       // in-flight runs per rule
}

func New(s *store.Store, r *actions.Runner, sm *secrets.Manager, log *slog.Logger) *Engine {
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
		if skip := e.claim(r); skip != "" {
			if _, err := e.store.StartRun(ctx, row, r, skip); err != nil {
				e.log.Error("record skipped run", "rule", r.Name, "err", err)
			}
			e.log.Info("rule skipped", "rule", r.Name, "reason", skip, "event", ev.Key())
			continue
		}
		e.startRun(row, r, ev)
	}
	e.log.Info("event received", "event", ev.Key(), "eventId", ev.EventID, "matched", len(res.Matched), "test", test)
	return res, nil
}

// RunRule runs one rule against ev now, ignoring enabled/trigger/cooldown.
// Used by the UI's "test" button. It blocks until the run finishes.
func (e *Engine) RunRule(ctx context.Context, r rules.Rule, ev yoursix.Event, raw []byte, sourceIP string) (store.RunView, error) {
	row, _, err := e.store.InsertEvent(ctx, ev, raw, sourceIP, true)
	if err != nil {
		return store.RunView{}, err
	}
	runID, err := e.store.StartRun(ctx, row, r, store.RunRunning)
	if err != nil {
		return store.RunView{}, err
	}
	steps, ok := e.runner.Run(ctx, r.Actions, ev.TemplateData(r.Name), e.secrets.LookupFor(ctx, r.UpdatedBy))
	status := store.RunOK
	if !ok {
		status = store.RunFailed
	}
	if err := e.store.FinishRun(context.WithoutCancel(ctx), runID, status, steps); err != nil {
		return store.RunView{}, err
	}
	return store.RunView{ID: runID, EventRow: row, RuleID: r.ID, RuleName: r.Name, Status: status, Steps: steps}, nil
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
			return
		}
		defer func() { <-e.sem }()

		steps, ok := e.runner.Run(e.ctx, r.Actions, ev.TemplateData(r.Name), e.secrets.LookupFor(e.ctx, r.UpdatedBy))
		status := store.RunOK
		if !ok {
			status = store.RunFailed
		}
		if err := e.store.FinishRun(context.Background(), runID, status, steps); err != nil {
			e.log.Error("finish run", "rule", r.Name, "err", err)
		}
		e.log.Info("rule ran", "rule", r.Name, "status", status, "event", ev.Key())
	}()
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
