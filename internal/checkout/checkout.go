// Package checkout keeps a local copy of each reviewed repository so the agent
// can read code beyond the diff. One blobless bare clone per repository holds
// the history; each review gets a throwaway worktree at the PR's head commit.
package checkout

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type Cache struct {
	Dir   string
	Token string // GitHub token for private repositories; sent as a header, never stored on disk
	// Remote returns the clone URL for a repository. Tests point it at a local path.
	Remote func(owner, name string) string

	mu sync.Mutex
}

func New(dir, token string) *Cache {
	return &Cache{Dir: dir, Token: token}
}

func (c *Cache) remote(owner, name string) string {
	if c.Remote != nil {
		return c.Remote(owner, name)
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

func (c *Cache) repoDir(owner, name string) string {
	return filepath.Join(c.Dir, "repos", owner, name+".git")
}

func (c *Cache) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if c.Token != "" {
		// Through the environment so the token never shows up in `ps` or on disk.
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+c.Token)))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = "…" + msg[len(msg)-300:]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

// ensureRepo creates the bare clone on first use. It is blobless: history
// comes down once, file contents only when a worktree needs them.
func (c *Cache) ensureRepo(ctx context.Context, owner, name string) (string, error) {
	dir := c.repoDir(owner, name)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := c.git(ctx, dir, "init", "--bare", "--quiet"); err != nil {
		return "", err
	}
	if _, err := c.git(ctx, dir, "remote", "add", "origin", c.remote(owner, name)); err != nil {
		return "", err
	}
	return dir, nil
}

// Worktree is a checkout of one PR head, removed with Remove.
type Worktree struct {
	Path string
	repo string
	base string
	c    *Cache
}

// Worktree fetches the PR head and the base branch, then checks the head out
// into a fresh directory. Only one checkout per PR exists at a time; a stale
// one left by a crash is replaced.
func (c *Cache) Worktree(ctx context.Context, owner, name string, number int, base, headSHA string) (*Worktree, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	repo, err := c.ensureRepo(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	pull := fmt.Sprintf("refs/pull/%d/head", number)
	_, err = c.git(ctx, repo, "fetch", "--quiet", "--no-tags", "--filter=blob:none", "origin",
		"+"+pull+":"+pull, fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(c.Dir, "work", fmt.Sprintf("%s-%s-%d", owner, name, number))
	_, _ = c.git(ctx, repo, "worktree", "remove", "--force", path)
	_ = os.RemoveAll(path)
	_, _ = c.git(ctx, repo, "worktree", "prune")
	at := headSHA
	if at == "" {
		at = pull
	}
	// Symlinks are written out as plain files holding the link target, so a
	// PR can't plant a link that points the reviewer at something outside
	// the checkout. The agent's Read refuses such paths anyway; this removes
	// the question.
	add := []string{"-c", "core.symlinks=false", "worktree", "add", "--quiet", "--detach", path}
	if _, err := c.git(ctx, repo, append(add, at)...); err != nil {
		// The head moved since the sync; review what GitHub serves under the PR ref.
		if _, err2 := c.git(ctx, repo, append(add, pull)...); err2 != nil {
			return nil, err
		}
	}
	return &Worktree{Path: path, repo: repo, base: base, c: c}, nil
}

// Remove deletes the checkout. The bare clone stays for the next review.
func (w *Worktree) Remove() {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	_, _ = w.c.git(context.Background(), w.repo, "worktree", "remove", "--force", w.Path)
	_ = os.RemoveAll(w.Path)
}

// BaseFile returns a file as it is on the base branch, so repository-provided
// context comes from what is merged rather than from the PR under review.
func (w *Worktree) BaseFile(ctx context.Context, path string) (string, error) {
	return w.c.git(ctx, w.repo, "show", fmt.Sprintf("refs/remotes/origin/%s:%s", w.base, path))
}

// Conventions returns the repository's agent instructions from the base
// branch (CLAUDE.md or AGENTS.md at the root), or "" when there are none.
func (w *Worktree) Conventions(ctx context.Context) string {
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if s, err := w.BaseFile(ctx, name); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Available reports whether git is installed.
func Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git not found on PATH")
	}
	return nil
}
