package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"yoursix-ifttt/internal/actions"
	"yoursix-ifttt/internal/config"
	"yoursix-ifttt/internal/engine"
	"yoursix-ifttt/internal/mocktarget"
	"yoursix-ifttt/internal/secrets"
	"yoursix-ifttt/internal/store"
	"yoursix-ifttt/internal/web"
	"yoursix-ifttt/internal/yoursix"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "healthcheck":
		err = healthcheck()
	case "mock-target":
		err = mockTarget(log)
	case "fire":
		err = fire(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (want serve, healthcheck, mock-target, fire, version)", cmd)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DevAuth {
		log.Warn("Y6IFTTT_DEV_AUTH is enabled: every local request is treated as an admin. Never use this behind a tunnel.")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "y6ifttt.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := st.FailInterruptedRuns(ctx); err != nil {
		return err
	}
	if cfg.SeedRulesDir != "" {
		n, err := st.SeedRules(ctx, cfg.SeedRulesDir)
		if err != nil {
			return fmt.Errorf("seed rules: %w", err)
		}
		if n > 0 {
			log.Info("seeded rules", "count", n, "dir", cfg.SeedRulesDir)
		}
	}

	var box *secrets.Box
	if cfg.SecretsKey != "" {
		key, err := secrets.ParseKey(cfg.SecretsKey)
		if err != nil {
			return err
		}
		if box, err = secrets.NewBox(key); err != nil {
			return err
		}
	} else {
		log.Warn("Y6IFTTT_SECRETS_KEY is not set: the secrets manager is disabled (env Y6IFTTT_SECRET_* still work)")
	}
	sm := secrets.NewManager(box, st)

	eng := engine.New(st, actions.NewRunner(cfg.DiscordWebhook, version), sm, log)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           web.New(cfg, st, eng, sm, log, version).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Rule test runs are synchronous and may include delays.
		WriteTimeout: 11 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	go prune(ctx, st, cfg.EventRetention, log)

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "version", version)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	eng.Shutdown(shutdownCtx)
	return nil
}

func prune(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.Prune(ctx, time.Now().Add(-retention)); err != nil {
			log.Error("prune events", "err", err)
		} else if n > 0 {
			log.Info("pruned old events", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// healthcheck is used by the container HEALTHCHECK (the runtime image has no curl).
func healthcheck() error {
	addr := os.Getenv("Y6IFTTT_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func mockTarget(log *slog.Logger) error {
	addr := os.Getenv("MOCK_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	user, pass := os.Getenv("MOCK_USER"), os.Getenv("MOCK_PASS")
	if user == "" {
		user, pass = "root", "pass"
	}
	log.Info("mock target listening", "addr", addr)
	srv := &http.Server{Addr: addr, Handler: mocktarget.New(user, pass, log), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// fire sends a webhook exactly as YourSix would, for testing:
//
//	y6ifttt fire <webhook-url> [topic:code | payload.json]
//
// A fresh eventId is generated each time so deduplication never hides it.
func fire(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: y6ifttt fire <webhook-url> [topic:code | payload.json]")
	}
	target, what := args[0], "device:vmd"
	if len(args) > 1 {
		what = args[1]
	}

	var ev yoursix.Event
	if strings.HasSuffix(what, ".json") {
		b, err := os.ReadFile(what)
		if err != nil {
			return err
		}
		if ev, err = yoursix.Parse(b); err != nil {
			return err
		}
		ev.EventID = yoursix.Sample(ev.EventTopic, ev.EventCode).EventID
		ev.EventOccurred = time.Now().Format(time.RFC3339)
	} else {
		topic, code, ok := strings.Cut(what, ":")
		if !ok {
			return fmt.Errorf("want topic:code, got %q", what)
		}
		ev = yoursix.Sample(topic, code)
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", yoursix.UserAgent)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Printf("%s %s -> %d %s\n", ev.Key(), ev.EventID, resp.StatusCode, strings.TrimSpace(string(out)))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}
