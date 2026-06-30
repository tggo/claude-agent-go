package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Run("empty baseDir defaults to tempdir/claude-agent", func(t *testing.T) {
		w := New("", nil)
		want := filepath.Join(os.TempDir(), "claude-agent")
		if w.BaseDir != want {
			t.Errorf("BaseDir = %q, want %q", w.BaseDir, want)
		}
		if w.Logger == nil {
			t.Error("Logger is nil, want default")
		}
	})

	t.Run("explicit baseDir kept", func(t *testing.T) {
		w := New("/custom/base", nil)
		if w.BaseDir != "/custom/base" {
			t.Errorf("BaseDir = %q", w.BaseDir)
		}
	})
}

func TestProjectDir(t *testing.T) {
	w := New("/base", nil)
	if got := w.ProjectDir("proj1"); got != filepath.Join("/base", "proj1") {
		t.Errorf("ProjectDir = %q", got)
	}
}

func TestEnsureProjectDir(t *testing.T) {
	w := New(t.TempDir(), nil)
	dir, err := w.EnsureProjectDir("proj1")
	if err != nil {
		t.Fatalf("EnsureProjectDir: %v", err)
	}
	if dir != w.ProjectDir("proj1") {
		t.Errorf("dir = %q, want %q", dir, w.ProjectDir("proj1"))
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("expected dir to exist, stat err=%v", err)
	}
}

func TestEnsureProjectDirError(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w := New(notADir, nil)
	if _, err := w.EnsureProjectDir("proj1"); err == nil {
		t.Error("expected error when base is a file, got nil")
	}
}

func TestSessionDir(t *testing.T) {
	base := t.TempDir()
	w := New(base, nil)
	dir, err := w.SessionDir("proj1", "sess1")
	if err != nil {
		t.Fatalf("SessionDir: %v", err)
	}
	want := filepath.Join(base, "proj1", ".sessions", "sess1")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Errorf("expected session dir to exist, stat err=%v", err)
	}
}

func TestSessionDirError(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w := New(notADir, nil)
	if _, err := w.SessionDir("proj1", "sess1"); err == nil {
		t.Error("expected error when base is a file, got nil")
	}
}

func TestWriteClaudeMD(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, nil)
	content := "# Project rules\nbe nice"
	path, err := w.WriteClaudeMD(dir, content)
	if err != nil {
		t.Fatalf("WriteClaudeMD: %v", err)
	}
	if path != filepath.Join(dir, "CLAUDE.md") {
		t.Errorf("path = %q", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestWriteClaudeMDError(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	w := New(dir, nil)
	if _, err := w.WriteClaudeMD(notADir, "content"); err == nil {
		t.Error("expected error writing CLAUDE.md under a file, got nil")
	}
}

func TestTouchLastAccessed(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, nil)
	w.TouchLastAccessed(dir)
	if _, err := os.Stat(filepath.Join(dir, lastAccessedFile)); err != nil {
		t.Errorf("expected .last_accessed to exist: %v", err)
	}
}

func TestTouchLastAccessedBadDir(t *testing.T) {
	w := New(t.TempDir(), nil)
	// Must not panic on a nonexistent dir; error is only logged.
	w.TouchLastAccessed(filepath.Join(t.TempDir(), "does", "not", "exist"))
}

func TestRepoName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://github.com/org/foo.git", "foo"},
		{"git@github.com:org/foo.git", "foo"},
		{"foo", "foo"},
		{"foo.git", "foo"},
		{"/path/to/bar.git", "bar"},
		{"/path/to/bar", "bar"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := RepoName(tt.in); got != tt.want {
				t.Errorf("RepoName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRunGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	w := New(t.TempDir(), nil)
	ctx := context.Background()

	out, err := w.RunGit(ctx, t.TempDir(), "--version")
	if err != nil {
		t.Fatalf("RunGit --version: %v", err)
	}
	if !strings.Contains(out, "git version") {
		t.Errorf("output = %q, want to contain 'git version'", out)
	}

	if _, err := w.RunGit(ctx, t.TempDir(), "definitely-not-a-cmd"); err == nil {
		t.Error("expected error for bogus subcommand, got nil")
	}
}

func TestWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	repoDir := t.TempDir()
	w := New(t.TempDir(), nil)

	mustGit := func(args ...string) {
		t.Helper()
		if _, err := w.RunGit(ctx, repoDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	mustGit("init")
	mustGit("config", "user.email", "test@example.com")
	mustGit("config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	mustGit("add", "file.txt")
	mustGit("commit", "-m", "initial")

	worktreeDir, branch, err := w.CreateWorktree(ctx, repoDir, "abcd1234ef")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	if branch != "temp/wt-abcd1234" {
		t.Errorf("branch = %q, want temp/wt-abcd1234", branch)
	}
	if info, err := os.Stat(worktreeDir); err != nil || !info.IsDir() {
		t.Errorf("worktree dir missing: stat err=%v", err)
	}

	w.RemoveWorktree(ctx, repoDir, worktreeDir)
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("expected worktree removed, stat err=%v", err)
	}
}

func TestCreateWorktreeError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	w := New(t.TempDir(), nil)
	// Not a git repo -> error.
	if _, _, err := w.CreateWorktree(context.Background(), t.TempDir(), "shortid"); err == nil {
		t.Error("expected error creating worktree in non-repo, got nil")
	}
}

func TestRemoveWorktreeNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	w := New(t.TempDir(), nil)
	// Must not panic / must tolerate failures (logged, not returned).
	w.RemoveWorktree(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "wt-x"))
}
