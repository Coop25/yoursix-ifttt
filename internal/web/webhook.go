package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"yoursix-ifttt/internal/yoursix"
)

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Webhook.Token)) != 1 {
		http.NotFound(w, r)
		return
	}
	ip := s.clientIP(r)
	if s.cfg.Webhook.RequireUserAgent && !yoursix.IsPlatformUserAgent(r.UserAgent()) {
		s.log.Warn("webhook rejected: user agent", "ua", r.UserAgent(), "ip", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if s.cfg.Webhook.VerifySourceIP && !s.sources.allowed(r.Context(), ip) {
		s.log.Warn("webhook rejected: source ip", "ip", ip)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ev, err := yoursix.Parse(body)
	if err != nil {
		s.log.Warn("webhook rejected: bad payload", "err", err, "ip", ip)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.engine.Dispatch(r.Context(), ev, body, ip, false)
	if err != nil {
		s.log.Error("dispatch", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": res.Duplicate, "matched": len(res.Matched)})
}

// clientIP honours the configured forwarding header (e.g. CF-Connecting-IP
// from cloudflared) and otherwise uses the TCP peer address.
func (s *Server) clientIP(r *http.Request) string {
	if h := s.cfg.ClientIPHeader; h != "" {
		if v := strings.TrimSpace(strings.Split(r.Header.Get(h), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sourceVerifier caches the A record of webhook.cloud.yoursix.com.
type sourceVerifier struct {
	host string
	ttl  time.Duration
	log  *slog.Logger

	mu      sync.Mutex
	ips     map[string]struct{}
	fetched time.Time
}

func newSourceVerifier(log *slog.Logger) *sourceVerifier {
	return &sourceVerifier{host: yoursix.WebhookHost, ttl: 10 * time.Minute, log: log}
}

func (v *sourceVerifier) allowed(ctx context.Context, ip string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ips == nil || time.Since(v.fetched) > v.ttl {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", v.host)
		if err != nil {
			v.log.Error("resolve webhook source host", "host", v.host, "err", err)
			// Keep using the last good set; fail closed if there never was one.
			if v.ips == nil {
				return false
			}
		} else {
			v.ips = map[string]struct{}{}
			for _, a := range addrs {
				v.ips[a.String()] = struct{}{}
			}
			v.fetched = time.Now()
		}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	_, ok := v.ips[parsed.String()]
	return ok
}
