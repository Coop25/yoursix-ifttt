package web

import (
	"errors"
	"fmt"
	"net/http"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
)

// checkSchedule verifies the schedule a rule picks exists.
func (s *Server) checkSchedule(r *http.Request, rule rules.Rule) error {
	if rule.ScheduleID == 0 {
		return nil
	}
	if _, err := s.store.Schedule(r.Context(), rule.ScheduleID); errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("schedule #%d doesn't exist", rule.ScheduleID)
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSchedules(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	sc, err := s.store.GetSchedule(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var sc rules.Schedule
	if !decodeBody(w, r, &sc) {
		return
	}
	by := editorFor(userFrom(r.Context()))
	created, err := s.store.CreateSchedule(r.Context(), sc, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("schedule created", "name", created.Name, "by", by.Name)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var sc rules.Schedule
	if !decodeBody(w, r, &sc) {
		return
	}
	sc.ID = id
	by := editorFor(userFrom(r.Context()))
	updated, err := s.store.UpdateSchedule(r.Context(), sc, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("schedule updated", "name", updated.Name, "by", by.Name)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	err := s.store.DeleteSchedule(r.Context(), id, editorFor(userFrom(r.Context())))
	var inUse store.ErrInUse
	switch {
	case errors.As(err, &inUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": inUse.Error(), "usedBy": inUse.Rules})
		return
	case err != nil:
		s.storeError(w, err)
		return
	}
	s.log.Info("schedule deleted", "id", id, "by", userFrom(r.Context()).Username)
	w.WriteHeader(http.StatusNoContent)
}
