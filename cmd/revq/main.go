package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/msyavuz/revq/internal/agent"
	"github.com/msyavuz/revq/internal/github"
	"github.com/msyavuz/revq/internal/pipeline"
	"github.com/msyavuz/revq/internal/store"
	"github.com/msyavuz/revq/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// githubToken prefers GITHUB_TOKEN and falls back to a logged-in gh CLI.
func githubToken() string {
	if t := env("GITHUB_TOKEN", os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token := githubToken()
	if token == "" {
		return errors.New("no GitHub token: set GITHUB_TOKEN or log in with gh")
	}
	st, err := store.Open(env("REVQ_DB", "revq.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ag, err := agent.NewClaudeCode(os.Getenv("REVQ_CLAUDE_BIN"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eng := pipeline.New(st, github.New(token), ag, log)
	eng.Start(ctx)

	addr := env("REVQ_ADDR", "127.0.0.1:8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           web.New(st, eng, log, os.Getenv("REVQ_PASSWORD")),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
