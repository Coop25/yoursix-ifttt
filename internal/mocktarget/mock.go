// Package mocktarget is a tiny HTTP server for the test environment. It
// stands in for a LAN camera: it logs every request, optionally enforces
// basic or digest auth, and keeps the last requests for inspection.
package mocktarget

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Request struct {
	Time    time.Time         `json:"time"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body,omitempty"`
}

type Server struct {
	user, pass string
	log        *slog.Logger

	mu   sync.Mutex
	last []Request
}

func New(user, pass string, log *slog.Logger) *Server {
	return &Server{user: user, pass: pass, log: log}
}

const realm = "AXIS_MOCK"
const nonce = "mock-nonce-0123456789abcdef"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_requests" {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.last)
		return
	}

	// /digest/... requires digest auth, /basic/... requires basic auth,
	// anything else is open. Mirrors how VAPIX endpoints behave.
	switch {
	case strings.HasPrefix(r.URL.Path, "/digest/"):
		if !s.checkDigest(r) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="%s", qop="auth", algorithm=MD5`, realm, nonce))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	case strings.HasPrefix(r.URL.Path, "/basic/"):
		u, p, ok := r.BasicAuth()
		if !ok || u != s.user || p != s.pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="mock"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	headers := map[string]string{}
	for k, v := range r.Header {
		if k != "Authorization" {
			headers[k] = strings.Join(v, ", ")
		}
	}
	req := Request{Time: time.Now(), Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: headers, Body: string(body)}
	s.mu.Lock()
	s.last = append(s.last, req)
	if len(s.last) > 100 {
		s.last = s.last[len(s.last)-100:]
	}
	s.mu.Unlock()
	s.log.Info("mock target hit", "method", r.Method, "path", r.URL.Path, "query", r.URL.RawQuery, "body", string(body))

	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "OK\n")
}

func (s *Server) checkDigest(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(h, "Digest "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			p[k] = strings.Trim(v, `"`)
		}
	}
	md := func(s string) string { x := md5.Sum([]byte(s)); return hex.EncodeToString(x[:]) }
	ha1 := md(s.user + ":" + realm + ":" + s.pass)
	ha2 := md(r.Method + ":" + p["uri"])
	want := md(ha1 + ":" + nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":" + p["qop"] + ":" + ha2)
	return p["username"] == s.user && p["response"] == want
}
