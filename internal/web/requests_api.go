package web

import (
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
		if a.Type != rules.ActionRequest {
			continue
		}
		sr, err := s.store.SavedRequest(r.Context(), a.RequestID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("action %d: saved action #%d doesn't exist", i+1, a.RequestID)
		}
		if err != nil {
			return err
		}
		refs, err := actions.SecretRefs([]rules.Action{sr.Request})
		if err != nil {
			return fmt.Errorf("action %d: %w", i+1, err)
		}
		if len(refs) > 0 && sr.UpdatedBy != editorID {
			return fmt.Errorf("action %d: saved action %q uses secrets belonging to %s, so only their rules can use it", i+1, sr.Name, nameOr(sr.UpdatedByName, "another admin"))
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
			return fmt.Errorf("action %d: %w", i+1, err)
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
// saving it (who becomes its owner), like rules.
func (s *Server) requestEditor(w http.ResponseWriter, r *http.Request, sr rules.SavedRequest) (store.Editor, bool) {
	u := userFrom(r.Context())
	if err := s.secrets.CheckRule(r.Context(), u.ID, []rules.Action{sr.Request}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	if err := actions.ParamSecretRefs([]rules.Action{action}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A throwaway rule owned by the tester: a request that uses secrets only
	// runs if the tester owns it, exactly as in a real rule.
	rule := rules.Rule{Name: "Saved action test: " + sr.Name, Actions: []rules.Action{action}, UpdatedBy: u.ID, UpdatedByName: u.DisplayName}
	ev := yoursix.Sample(yoursix.TopicDevice, "vmd")
	ev.EventID = ""
	raw, _ := json.Marshal(ev)
	run, err := s.engine.StartTestRun(r.Context(), rule, ev, raw, "test:"+u.Username)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run, "event": ev})
}
