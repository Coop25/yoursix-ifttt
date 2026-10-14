package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"

	"yoursix-ifttt/internal/rules"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/yoursix"
)

type ctxKey struct{}

func withUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func userFrom(ctx context.Context) User {
	u, _ := ctx.Value(ctxKey{}).(User)
	return u
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": userFrom(r.Context()), "version": s.version, "devAuth": s.cfg.DevAuth})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.PublicURL
	if base == "" {
		base = "https://<your-tunnel-hostname>"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhookUrl":       base + "/hook/" + s.cfg.Webhook.Token,
		"requireUserAgent": s.cfg.Webhook.RequireUserAgent,
		"verifySourceIp":   s.cfg.Webhook.VerifySourceIP,
		"clientIpHeader":   s.cfg.ClientIPHeader,
		"retentionDays":    int(s.cfg.EventRetention.Hours() / 24),
		"secretsEnabled":   s.secrets.Enabled(),
	})
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	topics := make([]string, 0, len(yoursix.Catalog))
	for t := range yoursix.Catalog {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	fields := []string{
		"eventCode", "eventTopic", "eventDescription", "eventId",
		"siteId", "siteName", "groupId", "groupName", "groupState", "groupActive",
		"deviceId", "deviceName", "deviceDescription", "videoSourceId",
		"barrierId", "barrierName", "peripheralId", "peripheralName", "sensorId", "sensorName",
	}
	writeJSON(w, http.StatusOK, map[string]any{"topics": topics, "codes": yoursix.Catalog, "fields": fields})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	var ve store.ValidationError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Error())
	default:
		s.log.Error("store", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleListRules(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListRules(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	for i := range list {
		list[i].Running = s.engine.Running(list[i].ID)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	rule, err := s.store.GetRule(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var rule rules.Rule
	if !decodeBody(w, r, &rule) {
		return
	}
	by, ok := s.ruleEditor(w, r, rule)
	if !ok {
		return
	}
	created, err := s.store.CreateRule(r.Context(), rule, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("rule created", "rule", created.Name, "by", userFrom(r.Context()).Username)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	var rule rules.Rule
	if !decodeBody(w, r, &rule) {
		return
	}
	rule.ID = id
	by, ok := s.ruleEditor(w, r, rule)
	if !ok {
		return
	}
	updated, err := s.store.UpdateRule(r.Context(), rule, by)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.log.Info("rule updated", "rule", updated.Name, "by", userFrom(r.Context()).Username)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.DeleteRule(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	s.engine.Forget(id)
	s.log.Info("rule deleted", "id", id, "by", userFrom(r.Context()).Username)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestRule runs one rule's actions immediately. The body may carry an
// "event" payload; otherwise a sample matching the rule's trigger is used.
func (s *Server) handleTestRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	rule, err := s.store.GetRule(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	var req struct {
		Event json.RawMessage `json:"event"`
	}
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}

	var ev yoursix.Event
	if len(req.Event) > 0 && string(req.Event) != "null" {
		if ev, err = yoursix.Parse(req.Event); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		topic, code := yoursix.TopicDevice, "vmd"
		if len(rule.Trigger.Topics) > 0 {
			topic = rule.Trigger.Topics[0]
		}
		if len(rule.Trigger.Codes) > 0 {
			code = rule.Trigger.Codes[0]
		}
		ev = yoursix.Sample(topic, code)
	}
	ev.EventID = "" // test runs never collide with real deliveries
	raw, _ := json.Marshal(ev)

	run, err := s.engine.RunRule(r.Context(), rule.Rule, ev, raw, "test:"+userFrom(r.Context()).Username)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "event": ev, "matchesTrigger": rule.Trigger.Matches(ev)})
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := s.store.ListEvents(r.Context(), limit, before)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	ev, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// handleReplayEvent re-dispatches a stored event through all enabled rules.
func (s *Server) handleReplayEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad id")
		return
	}
	stored, err := s.store.GetEvent(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.simulate(w, r, stored.Payload)
}

// handleSimulate dispatches an arbitrary payload as if YourSix had sent it.
func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad body")
		return
	}
	s.simulate(w, r, body)
}

func (s *Server) simulate(w http.ResponseWriter, r *http.Request, payload []byte) {
	ev, err := yoursix.Parse(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev.EventID = ""
	raw, _ := json.Marshal(ev)
	res, err := s.engine.Dispatch(r.Context(), ev, raw, "simulate:"+userFrom(r.Context()).Username, true)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
