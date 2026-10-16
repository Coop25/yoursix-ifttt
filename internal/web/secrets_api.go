package web

import (
	"errors"
	"net/http"
	"strings"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
)

// ruleEditor records who is saving a rule and checks that every secret the
// rule references belongs to them (or is an operator env secret). The saver
// becomes the rule's owner, and runs use only the owner's secrets.
func (s *Server) ruleEditor(w http.ResponseWriter, r *http.Request, rule rules.Rule) (store.Editor, bool) {
	u := userFrom(r.Context())
	if err := s.secrets.CheckRule(r.Context(), u.ID, rule.AllActions()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return store.Editor{}, false
	}
	if err := s.checkRequestActions(r, u.ID, rule.AllActions()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return store.Editor{}, false
	}
	if err := s.checkSchedule(r, rule); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return store.Editor{}, false
	}
	return editorFor(u), true
}

func editorFor(u User) store.Editor {
	name := u.DisplayName
	if name == "" {
		name = u.Username
	}
	return store.Editor{ID: u.ID, Name: name}
}

// Secrets are scoped to the signed-in admin: every handler uses the session's
// user ID, never one from the request, and no handler returns a value.

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.secrets.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "secrets": []any{}, "error": secrets.ErrDisabled.Error()})
		return
	}
	list, err := s.secrets.List(r.Context(), userFrom(r.Context()).ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "secrets": list})
}

func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Value string `json:"value"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	u := userFrom(r.Context())
	name, err := s.secrets.Put(r.Context(), u.ID, u.Username, r.PathValue("name"), req.Value)
	switch {
	case errors.Is(err, secrets.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case errors.Is(err, secrets.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		s.storeError(w, err)
		return
	}
	// Log the name only, never the value.
	s.log.Info("secret saved", "name", name, "owner", u.Username)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := s.secrets.Delete(r.Context(), u.ID, r.PathValue("name")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if errors.Is(err, secrets.ErrInvalid) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.storeError(w, err)
		return
	}
	s.log.Info("secret deleted", "name", strings.ToUpper(r.PathValue("name")), "owner", u.Username)
	w.WriteHeader(http.StatusNoContent)
}
