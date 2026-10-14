package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"yoursix-ifttt/internal/store"
)

// Live updates for the Events tab, pushed over Server-Sent Events instead of
// the page polling. The engine reports which event changed; the hub batches
// those, loads each changed event once, and sends the same row the list API
// returns (without the payload) to every open stream.

const (
	liveBatchDelay   = 200 * time.Millisecond // coalesce bursts (e.g. step progress)
	liveKeepalive    = 25 * time.Second       // under Cloudflare's 100s idle timeout
	liveWriteTimeout = 15 * time.Second
	liveMaxStreams   = 200
	liveBuffer       = 64
)

var errTooManyStreams = errors.New("too many live connections")

type liveHub struct {
	store *store.Store
	log   *slog.Logger

	mu      sync.Mutex
	subs    map[chan []byte]struct{}
	pending map[int64]struct{}

	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newLiveHub(st *store.Store, log *slog.Logger) *liveHub {
	h := &liveHub{
		store:   st,
		log:     log,
		subs:    map[chan []byte]struct{}{},
		pending: map[int64]struct{}{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go h.loop()
	return h
}

// Notify marks an event as changed. Cheap and non-blocking; safe to call
// from the engine's goroutines.
func (h *liveHub) Notify(eventRow int64) {
	h.mu.Lock()
	if len(h.subs) == 0 {
		// Nobody is listening; skip the database work entirely.
		h.mu.Unlock()
		return
	}
	h.pending[eventRow] = struct{}{}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *liveHub) loop() {
	for {
		select {
		case <-h.done:
			return
		case <-h.wake:
		}
		// Let a burst of changes (a run's steps, several rules) settle first.
		select {
		case <-h.done:
			return
		case <-time.After(liveBatchDelay):
		}

		h.mu.Lock()
		rows := make([]int64, 0, len(h.pending))
		for row := range h.pending {
			rows = append(rows, row)
		}
		clear(h.pending)
		h.mu.Unlock()

		for _, row := range rows {
			ev, err := h.store.GetEvent(context.Background(), row)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				h.log.Error("live update", "event", row, "err", err)
				continue
			}
			ev.Payload = nil // the list shape; details are fetched on expand
			msg, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			h.broadcast(msg)
		}
	}
}

// broadcast sends msg to every stream. A stream too slow to keep up is
// dropped; its browser reconnects and reloads the list to catch up.
func (h *liveHub) broadcast(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *liveHub) subscribe() (chan []byte, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= liveMaxStreams {
		return nil, nil, errTooManyStreams
	}
	ch := make(chan []byte, liveBuffer)
	h.subs[ch] = struct{}{}
	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
	return ch, unsubscribe, nil
}

// Close ends every stream (on server shutdown, so http.Server.Shutdown
// isn't held up by long-lived connections).
func (h *liveHub) Close() {
	h.closeOnce.Do(func() { close(h.done) })
}

// handleEventStream is GET /api/events/stream: a Server-Sent Events stream
// of changed events for any signed-in user (viewers included, like the list).
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	ch, unsubscribe, err := s.live.subscribe()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer unsubscribe()

	rc := http.NewResponseController(w)
	// The server's read/write timeouts are for normal requests; this one
	// stays open, so lift the read deadline and renew the write deadline per
	// message instead.
	_ = rc.SetReadDeadline(time.Time{})

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// retry: how long the browser waits before reconnecting.
	if !send("retry: 3000\n: connected\n\n") {
		return
	}

	keepalive := time.NewTicker(liveKeepalive)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.live.done:
			return
		case msg, ok := <-ch:
			if !ok {
				return // dropped as too slow; the client reconnects
			}
			if !send("event: event\ndata: " + string(msg) + "\n\n") {
				return
			}
		case <-keepalive.C:
			// Re-check the session so an expired or revoked login doesn't
			// keep receiving events on an old connection.
			if _, ok := s.auth.userFromRequest(r); !ok {
				send("event: signedout\ndata: {}\n\n")
				return
			}
			if !send(": ping\n\n") {
				return
			}
		}
	}
}
