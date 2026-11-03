package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

// checkRequestActions validates a rule's "request" actions for the admin
// saving it: the saved request exists, its parameters are satisfied, and if
// it uses secrets it belongs to that admin (see actions.runSavedRequest).
func (s *Server) checkRequestActions(r *http.Request, editorID string, acts []rules.Action) error {
	for i, a := range acts {
		if a.Type == rules.ActionDelay && a.Watch != nil {
			if err := s.checkNestedWatch(r.Context(), a.Watch); err != nil {
				return &rules.ActionError{Index: i, Err: err}
			}
		}
		if a.Type != rules.ActionRequest {
			continue
		}
		sr, err := s.store.SavedRequest(r.Context(), a.RequestID)
		if errors.Is(err, store.ErrNotFound) {
			return &rules.ActionError{Index: i, Err: fmt.Errorf("saved action #%d doesn't exist", a.RequestID)}
		}
		if err != nil {
			return err
		}
		refs, err := actions.SavedSecretRefs(r.Context(), s.store, sr)
		if err != nil {
			return &rules.ActionError{Index: i, Err: err}
		}
		if len(refs) > 0 && sr.UpdatedBy != editorID {
			return &rules.ActionError{Index: i, Err: fmt.Errorf("saved action %q uses secrets belonging to %s, so only their rules and saved actions can use it", sr.Name, nameOr(sr.UpdatedByName, "another admin"))}
		}
		// Templated values count as provided; only presence is checked here.
		present := make(map[string]string, len(a.Params))
		for k, v := range a.Params {
			if v != "" {
				present[k] = "x"
			} else {
				present[k] = ""
			}
		}
		if _, err := sr.ResolveParams(present); err != nil {
			return &rules.ActionError{Index: i, Err: err}
		}
	}
	return nil
}

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListRequests(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	sr, err := s.store.GetRequest(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sr)
}

// requestEditor checks the secrets a saved request uses belong to the admin
// saving it (who becomes its owner), like rules, including those of saved
// actions its watch runs.
func (s *Server) requestEditor(w http.ResponseWriter, r *http.Request, sr rules.SavedRequest) (store.Editor, bool) {
	u := userFrom(r.Context())
	if err := s.secrets.CheckRule(r.Context(), u.ID, sr.AllActions()); err != nil {
		writeError(w, http.StatusBadRequest, sr.Relabel(err).Error())
		return store.Editor{}, false
	}
	// This also rejects a watch that runs another watching saved delay
	// (checkNestedWatch, for the saved delay itself at index 0).
	if err := s.checkRequestActions(r, u.ID, sr.AllActions()); err != nil {
		writeError(w, http.StatusBadRequest, sr.Relabel(err).Error())
		return store.Editor{}, false
	}
	return editorFor(u), true
}

func (s *Server) handleCreateRequest(w http.ResponseWriter, r *http.Request) {
	var sr rules.SavedRequest
	if !decodeBody(w, r, &sr) {
		return
	}
	by, ok := s.requestEditor(w, r, sr)
	if !ok {
		return
	}
	created, err := s.store.CreateRequest(r.Context(), sr, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("saved request created", "name", created.Name, "by", by.Name)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var sr rules.SavedRequest
	if !decodeBody(w, r, &sr) {
		return
	}
	sr.ID = id
	by, ok := s.requestEditor(w, r, sr)
	if !ok {
		return
	}
	if sr.Request.Watch != nil {
		if err := s.checkStartsWatching(r.Context(), id, sr.Request.Watch); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	updated, err := s.store.UpdateRequest(r.Context(), sr, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("saved request updated", "name", updated.Name, "by", by.Name)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	err := s.store.DeleteRequest(r.Context(), id, editorFor(userFrom(r.Context())))
	var inUse store.ErrInUse
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "usedBy": inUse.Rules})
		return
	case err != nil:
		s.storeError(w, err)
		return
	}
	s.log.Info("saved request deleted", "id", id, "by", userFrom(r.Context()).Username)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestRequest runs a saved request on its own, in the background like
// a rule test, with a sample event and the given parameter values.
func (s *Server) handleTestRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	sr, err := s.store.GetRequest(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	var req struct {
		Params map[string]string `json:"params"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	action := rules.Action{Type: rules.ActionRequest, RequestID: id, Params: req.Params}
	if err := actions.ForbiddenSecretRefs([]rules.Action{action}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A throwaway rule owned by the tester: a request that uses secrets only
	// runs if the tester owns it, exactly as in a real rule.
	rule := rules.Rule{Name: "Saved action test: " + sr.Name, Actions: []rules.Action{action}, UpdatedBy: u.ID, UpdatedByName: u.DisplayName}
	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")
	ev.EventID = ""
	raw, _ := json.Marshal(ev)
	run, err := s.engine.StartSavedActionTestRun(r.Context(), rule, ev, raw, "test:"+u.Username)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run, "event": ev})
}

// checkNestedWatch rejects a watch whose actions call a saved delay that
// itself watches: only one delay may be watching at a time, so such a call
// could never run.
func (s *Server) checkNestedWatch(ctx context.Context, w *rules.Watch) error {
	lists := []struct {
		label string
		acts  []rules.Action
	}{{"on-match", w.Actions}, {"if-no-match", w.IfNoMatch}}
	for _, l := range lists {
		for i, a := range l.acts {
			if a.Type != rules.ActionRequest {
				continue
			}
			sr, err := s.store.SavedRequest(ctx, a.RequestID)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("watch: %s action %d: saved action #%d doesn't exist", l.label, i+1, a.RequestID)
			}
			if err != nil {
				return err
			}
			if sr.Request.Watch != nil {
				return fmt.Errorf("watch: %s action %d: saved action %q watches for events, so it can't run while another delay is watching", l.label, i+1, sr.Name)
			}
		}
	}
	return nil
}

// checkStartsWatching rejects giving saved action id a watch while it is run
// where a watching wait can't run: by another delay's watch (in a rule or a
// saved wait) or by a rule's on-failure steps. Saving those callers checks
// the same thing (checkNestedWatch, checkOnFailureWatch); this covers the
// saved action changing under them. own is the saved action's new watch,
// which can't run the saved action itself either.
func (s *Server) checkStartsWatching(ctx context.Context, id int64, own *rules.Watch) error {
	calls := func(acts []rules.Action) bool {
		for _, a := range acts {
			if a.Type == rules.ActionRequest && a.RequestID == id {
				return true
			}
		}
		return false
	}
	watchCalls := func(w *rules.Watch) bool { return w != nil && (calls(w.Actions) || calls(w.IfNoMatch)) }
	if watchCalls(own) {
		return errors.New("watch: its actions can't run this saved action itself, since it watches for events")
	}
	all, err := s.store.ListRules(ctx)
	if err != nil {
		return err
	}
	for _, rv := range all {
		if calls(rv.OnFailure) {
			return fmt.Errorf("rule %q runs this in \"If something goes wrong\", where a wait can't watch for events; remove it there first", rv.Name)
		}
		for _, a := range rv.Actions {
			if watchCalls(a.Watch) {
				return fmt.Errorf("rule %q runs this while another delay is watching, where it can't watch for events itself; remove it there first", rv.Name)
			}
		}
	}
	saved, err := s.store.ListRequests(ctx)
	if err != nil {
		return err
	}
	for _, other := range saved {
		if other.ID != id && watchCalls(other.Request.Watch) {
			return fmt.Errorf("saved wait %q runs this while it is watching, where it can't watch for events itself; remove it there first", other.Name)
		}
	}
	return nil
}

// checkOnFailureWatch rejects on-failure steps that run a saved delay that
// watches for events: on-failure steps can't watch.
func (s *Server) checkOnFailureWatch(ctx context.Context, onFailure []rules.Action) error {
	for i, a := range onFailure {
		if a.Type != rules.ActionRequest {
			continue
		}
		sr, err := s.store.SavedRequest(ctx, a.RequestID)
		if errors.Is(err, store.ErrNotFound) {
			continue // checkRequestActions reports it
		}
		if err != nil {
			return err
		}
		if sr.Request.Watch != nil {
			return fmt.Errorf("on-failure action %d: %w", i+1, actions.OnFailureWatchError(sr.Name))
		}
	}
	return nil
}
