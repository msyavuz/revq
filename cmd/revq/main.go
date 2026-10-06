package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/msyavuz/revq/internal/update"
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

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// resetPassword is the way back in after a forgotten password.
func resetPassword() error {
	st, err := store.Open(env("REVQ_DB", "revq.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.ResetUser(); err != nil {
		return err
	}
	fmt.Println("Account reset. Sign in as admin / admin and set a new password.")
	return nil
}

// selfUpdate replaces this binary with the latest release. It does not
// restart anything; the service keeps running the old version until restarted.
func selfUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	latest, err := update.Latest(ctx)
	if err != nil {
		return fmt.Errorf("checking for the latest release: %w", err)
	}
	if latest == version {
		fmt.Println("revq", version, "is the latest release.")
		return nil
	}
	if !update.Newer(latest, version) && version != "dev" {
		fmt.Printf("This is revq %s; the latest release is %s. Nothing to do.\n", version, latest)
		return nil
	}
	path, err := update.Apply(ctx, latest)
	if err != nil {
		return err
	}
	fmt.Printf("Updated %s from %s to %s.\nRestart revq to run it, for example: systemctl restart revq\n", path, version, latest)
	return nil
}

func run(log *slog.Logger) error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "reset-password":
			return resetPassword()
		case "version", "--version", "-v":
			fmt.Println("revq", version)
			return nil
		case "update":
			return selfUpdate()
		default:
			return fmt.Errorf("unknown command %q (commands: version, update, reset-password)", os.Args[1])
		}
	}
	token := githubToken()
	if token == "" {
		return errors.New("no GitHub token: set GITHUB_TOKEN or log in with gh")
	}
	st, err := store.Open(env("REVQ_DB", "revq.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if u, err := st.User(); err != nil {
		return err
	} else if u.MustChange {
		log.Warn("account still has the default password; sign in as admin / admin to set a new one")
	}
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
		Handler:           web.New(st, eng, log, version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", addr, "version", version)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
