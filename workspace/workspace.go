// Package workspace manages on-disk scaffolding for Claude CLI runs: per-project
// and per-session directories, CLAUDE.md placement, git worktrees, and direct
// git invocations. It is generic and auth-free — authentication and repo
// cloning stay in the consuming application, which can layer them on top of
// RunGit.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// lastAccessedFile marks a project dir's recency for external cleanup logic.
const lastAccessedFile = ".last_accessed"

// Workspace roots all scratch directories under BaseDir. A zero Logger falls
// back to slog.Default().
type Workspace struct {
	// BaseDir is the root for all project/session directories (e.g. /tmp/agent).
	BaseDir string
	// Logger receives structured logs; defaults to slog.Default() when nil.
	Logger *slog.Logger
}

// New constructs a Workspace rooted at baseDir.
func New(baseDir string, logger *slog.Logger) *Workspace {
	if logger == nil {
		logger = slog.Default()
	}
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "claude-agent")
	}
	return &Workspace{BaseDir: baseDir, Logger: logger}
}

// ProjectDir returns BaseDir/projectID (without creating it).
func (w *Workspace) ProjectDir(projectID string) string {
	return filepath.Join(w.BaseDir, projectID)
}

// EnsureProjectDir creates BaseDir/projectID and returns its path.
func (w *Workspace) EnsureProjectDir(projectID string) (string, error) {
	dir := w.ProjectDir(projectID)
	//nolint:gosec // dir must be traversable by the CLI subprocess.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create project dir: %w", err)
	}
	return dir, nil
}

// SessionDir creates and returns BaseDir/projectID/.sessions/sessionID — the
// place to drop a per-session CLAUDE.md and MCP config.
func (w *Workspace) SessionDir(projectID, sessionID string) (string, error) {
	dir := filepath.Join(w.BaseDir, projectID, ".sessions", sessionID)
	//nolint:gosec // dir must be traversable by the CLI subprocess.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create session dir: %w", err)
	}
	return dir, nil
}

// WriteClaudeMD writes content to dir/CLAUDE.md and returns the path.
func (w *Workspace) WriteClaudeMD(dir, content string) (string, error) {
	path := filepath.Join(dir, "CLAUDE.md")
	//nolint:gosec // CLAUDE.md must be readable by the CLI subprocess.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write CLAUDE.md: %w", err)
	}
	return path, nil
}

// TouchLastAccessed stamps dir/.last_accessed with the current time so external
// cleanup can age out stale projects. Errors are logged, not returned.
func (w *Workspace) TouchLastAccessed(dir string) {
	path := filepath.Join(dir, lastAccessedFile)
	//nolint:gosec // marker file, non-sensitive.
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		w.Logger.Warn("workspace: touch last_accessed failed", "dir", dir, "err", err)
	}
}

// RunGit runs git with args in dir and returns trimmed stdout. On failure the
// error includes stderr (or stdout, where git writes some messages). Auth is
// the caller's concern — configure it via env or ~/.gitconfig before calling.
func (w *Workspace) RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	//nolint:gosec // args are supplied by the application, not end users.
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("git %s: %s: %w", firstArg(args), msg, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// CreateWorktree adds a git worktree off HEAD of the repo at repoDir, named for
// a short slug of id, and returns the worktree path and branch name. This lets
// parallel agent runs operate on isolated checkouts of the same repo.
func (w *Workspace) CreateWorktree(ctx context.Context, repoDir, id string) (worktreeDir, branch string, err error) {
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	worktreeDir = filepath.Join(filepath.Dir(repoDir), "wt-"+short)
	branch = "temp/wt-" + short

	if _, err = w.RunGit(ctx, repoDir, "worktree", "add", worktreeDir, "-b", branch); err != nil {
		return "", "", fmt.Errorf("create worktree: %w", err)
	}
	w.Logger.Info("workspace: created worktree", "repo", repoDir, "worktree", worktreeDir, "branch", branch)
	return worktreeDir, branch, nil
}

// RemoveWorktree force-removes a worktree and deletes its temp branch. Failures
// are logged, not returned, since the worktree may already be gone.
func (w *Workspace) RemoveWorktree(ctx context.Context, repoDir, worktreeDir string) {
	if _, err := w.RunGit(ctx, repoDir, "worktree", "remove", worktreeDir, "--force"); err != nil {
		w.Logger.Warn("workspace: remove worktree failed", "worktree", worktreeDir, "err", err)
	}
	branch := "temp/" + filepath.Base(worktreeDir)
	if _, err := w.RunGit(ctx, repoDir, "branch", "-D", branch); err != nil {
		w.Logger.Warn("workspace: delete temp branch failed", "branch", branch, "err", err)
	}
}

// RepoName extracts the repository name from a clone URL (".../foo.git" -> "foo").
func RepoName(repoURL string) string {
	return strings.TrimSuffix(filepath.Base(repoURL), ".git")
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
