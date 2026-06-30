// Command worktree-parallel fans out N agents over the same repo at once, each
// in its own isolated git worktree. They run concurrently on goroutines, each
// commits to its own branch, and none of them collide — the main checkout is
// never touched.
//
//	go run ./examples/worktree-parallel
//
// Needs `git` and the `claude` CLI on PATH. Builds a throwaway repo itself.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

type task struct {
	id      string // worktree slug + branch suffix
	file    string
	content string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	base, err := os.MkdirTemp("", "wt-par-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(base)

	// Quiet the workspace's info logs so the concurrent output stays readable.
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.New(base, quiet)

	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "agent@example.com"},
		{"config", "user.name", "Agent"},
	} {
		if _, err := ws.RunGit(ctx, repo, args...); err != nil {
			log.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# parallel demo\n"), 0o644)
	ws.RunGit(ctx, repo, "add", ".")
	ws.RunGit(ctx, repo, "commit", "-m", "initial commit")

	tasks := []task{
		{"docs", "DOCS.md", "one short sentence describing what documentation is"},
		{"changelog", "CHANGELOG.md", "a single changelog entry under a v0.1.0 heading"},
		{"contributing", "CONTRIBUTING.md", "two short bullet points on how to contribute"},
	}

	// Create the worktrees up front (sequential — git worktree add takes a repo
	// lock), then run the agents concurrently. The agent runs are the slow part.
	type job struct {
		task   task
		wt     string
		branch string
	}
	jobs := make([]job, 0, len(tasks))
	for _, t := range tasks {
		wt, branch, err := ws.CreateWorktree(ctx, repo, t.id)
		if err != nil {
			log.Fatalf("worktree %s: %v", t.id, err)
		}
		defer ws.RemoveWorktree(ctx, repo, wt)
		jobs = append(jobs, job{t, wt, branch})
	}

	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(8),
		runner.WithTimeout(3*time.Minute),
		runner.WithLogger(quiet),
	)

	fmt.Printf("running %d agents in parallel, one worktree each…\n\n", len(jobs))
	start := time.Now()

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			_, err := r.Run(ctx, runner.Input{
				WorkDir: j.wt,
				Prompt: fmt.Sprintf(
					"Create a file named %s containing %s. Then stage and commit it with git, message: add %s.",
					j.task.file, j.task.content, j.task.file),
			})
			if err != nil {
				fmt.Printf("  [%s] error: %v\n", j.task.id, err)
			}
		}(j)
	}
	wg.Wait()

	fmt.Printf("all agents finished in %s\n\n", time.Since(start).Round(time.Second))

	// Each branch has exactly its own new commit; main has none of them.
	for _, j := range jobs {
		head, _ := ws.RunGit(ctx, j.wt, "log", "--oneline", "-1")
		files, _ := ws.RunGit(ctx, j.wt, "ls-files")
		fmt.Printf("branch %-18s HEAD: %s\n  files: %v\n", j.branch, head, oneLine(files))
	}
	mlog, _ := ws.RunGit(ctx, repo, "log", "--oneline", "main")
	fmt.Printf("\nmain branch (untouched): %s\n", oneLine(mlog))
}

func oneLine(s string) string {
	out := ""
	for _, r := range s {
		if r == '\n' {
			out += " · "
			continue
		}
		out += string(r)
	}
	return out
}
