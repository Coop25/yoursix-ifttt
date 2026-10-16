// Package web serves the public webhook receiver and the Discord-gated UI/API.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"yoursix-ifttt/internal/config"
	"yoursix-ifttt/internal/engine"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg     config.Config
	store   *store.Store
	engine  *engine.Engine
	secrets *secrets.Manager
	auth    *auth
	sources *sourceVerifier
	live    *liveHub
	log     *slog.Logger
	version string
}

func New(cfg config.Config, st *store.Store, eng *engine.Engine, sm *secrets.Manager, log *slog.Logger, version string) *Server {
	s := &Server{
		cfg:     cfg,
		store:   st,
		engine:  eng,
		secrets: sm,
		auth:    newAuth(cfg.Auth, cfg.DevAuth),
		sources: newSourceVerifier(log),
		log:     log,
		version: version,
	}
	s.live = newLiveHub(st, log)
	s.live.decorate = s.withMatrix
	eng.OnChange = s.live.Notify
	return s
}

// Close ends long-lived live-update streams. Register it with
// http.Server.RegisterOnShutdown so shutdown isn't held up by them.
func (s *Server) Close() { s.live.Close() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: health check and the YourSix webhook receiver.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /hook/{token}", s.handleWebhook)

	// Login flow.
	mux.HandleFunc("GET /auth/login", s.auth.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.auth.handleCallback)
	mux.HandleFunc("POST /auth/logout", s.auth.handleLogout)

	// API: any allowed user may read; only admins may change things.
	view := s.requireRole(RoleViewer)
	admin := s.requireRole(RoleAdmin)
	mux.Handle("GET /api/me", view(s.handleMe))
	mux.Handle("GET /api/catalog", view(s.handleCatalog))
	mux.Handle("GET /api/info", admin(s.handleInfo))
	mux.Handle("GET /api/rules", view(s.handleListRules))
	mux.Handle("POST /api/rules", admin(s.handleCreateRule))
	mux.Handle("GET /api/rules/{id}", view(s.handleGetRule))
	mux.Handle("PUT /api/rules/{id}", admin(s.handleUpdateRule))
	mux.Handle("DELETE /api/rules/{id}", admin(s.handleDeleteRule))
	mux.Handle("POST /api/rules/{id}/test", admin(s.handleTestRule))
	mux.Handle("GET /api/runs/{id}", view(s.handleGetRun))
	mux.Handle("GET /api/events", view(s.handleListEvents))
	mux.Handle("GET /api/history", view(s.handleHistory))
	mux.Handle("GET /api/events/stream", view(s.handleEventStream))
	mux.Handle("GET /api/events/{id}", view(s.handleGetEvent))
	mux.Handle("POST /api/events/{id}/replay", admin(s.handleReplayEvent))
	mux.Handle("POST /api/simulate", admin(s.handleSimulate))
	mux.Handle("POST /api/preview", admin(s.handlePreview))
	mux.Handle("GET /api/requests", view(s.handleListRequests))
	mux.Handle("POST /api/requests", admin(s.handleCreateRequest))
	mux.Handle("GET /api/requests/{id}", view(s.handleGetRequest))
	mux.Handle("PUT /api/requests/{id}", admin(s.handleUpdateRequest))
	mux.Handle("DELETE /api/requests/{id}", admin(s.handleDeleteRequest))
	mux.Handle("POST /api/requests/{id}/test", admin(s.handleTestRequest))
	mux.Handle("GET /api/schedules", view(s.handleListSchedules))
	mux.Handle("POST /api/schedules", admin(s.handleCreateSchedule))
	mux.Handle("GET /api/schedules/{id}", view(s.handleGetSchedule))
	mux.Handle("PUT /api/schedules/{id}", admin(s.handleUpdateSchedule))
	mux.Handle("DELETE /api/schedules/{id}", admin(s.handleDeleteSchedule))
	mux.Handle("GET /api/secrets", admin(s.handleListSecrets))
	mux.Handle("PUT /api/secrets/{name}", admin(s.handlePutSecret))
	mux.Handle("DELETE /api/secrets/{name}", admin(s.handleDeleteSecret))

	// UI (static; all data comes from the gated API). Public, like the files
	// themselves, so a stale login page can be refreshed too; POST plus the
	// cross-origin check below keeps other sites from triggering it.
	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", staticHandler(sub))
	mux.HandleFunc("POST /ui/clear-cache", handleClearCache)
	// Some clients still ask for /favicon.ico without reading the <link>.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/favicon.svg", http.StatusMovedPermanently)
	})

	// Rejects cross-site state-changing browser requests (Sec-Fetch-Site /
	// Origin checks). Webhooks come from a server, not a browser, so they
	// carry neither header and pass.
	csrf := http.NewCrossOriginProtection()
	return securityHeaders(csrf.Handler(mux))
}

func (s *Server) requireRole(min Role) func(http.HandlerFunc) http.Handler {
	return func(next http.HandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := s.auth.userFromRequest(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "not signed in")
				return
			}
			if min == RoleAdmin && u.Role != RoleAdmin {
				writeError(w, http.StatusForbidden, "admin role required")
				return
			}
			next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
		})
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https://cdn.discordapp.com data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version, "time": time.Now().UTC()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
