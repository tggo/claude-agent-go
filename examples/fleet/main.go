// Command fleet fans a batch of tasks out across a pool of workers, each task
// running in its own isolated git worktree, with per-task retries and a
// fleet-wide spend cap. This is the "distributed runner" shape: swap a worker's
// runner transport for DockerExec/SSH and the same batch runs across machines.
//
//	go run ./examples/fleet
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
	"time"

	"github.com/tggo/claude-agent-go/fleet"
	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Throwaway repo so each task has something to worktree off.
	base, _ := os.MkdirTemp("", "fleet-")
	defer os.RemoveAll(base)
	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.New(base, quiet)
	repo := filepath.Join(base, "repo")
	os.MkdirAll(repo, 0o755)
	for _, a := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "a@b.c"}, {"config", "user.name", "Agent"}} {
		ws.RunGit(ctx, repo, a...)
	}
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fleet demo\n"), 0o644)
	ws.RunGit(ctx, repo, "add", ".")
	ws.RunGit(ctx, repo, "commit", "-m", "init")

	// Two local workers — same machine, two lanes. In production, give each a
	// different transport (DockerExec / SSH) to spread across hosts.
	r := runner.New(runner.WithModel("haiku"), runner.WithMaxTurns(6), runner.WithTimeout(3*time.Minute), runner.WithLogger(quiet))
	f, err := fleet.New(fleet.Config{
		Workers:     []fleet.Worker{{Name: "lane-1", Runner: r}, {Name: "lane-2", Runner: r}},
		Workspace:   ws,
		Repo:        repo,
		MaxSpendUSD: 1.00, // never spend more than $1 on this batch
		Retry:       &runner.RetryPolicy{MaxAttempts: 3, MaxSpendUSD: 0.30},
		OnResult: func(tr fleet.TaskResult) {
			if tr.Skipped {
				fmt.Printf("• %-12s SKIPPED (%v)\n", tr.TaskID, tr.Err)
				return
			}
			if tr.Err != nil {
				fmt.Printf("• %-12s [%s] FAILED: %v\n", tr.TaskID, tr.Worker, tr.Err)
				return
			}
			fmt.Printf("• %-12s [%s] $%.4f — %s\n", tr.TaskID, tr.Worker, tr.Result.TotalCostUSD, oneLine(tr.Result.ResultText))
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	tasks := []fleet.Task{
		{ID: "make-docs", Input: runner.Input{Prompt: "Create DOCS.md with one sentence, then git add and commit it."}},
		{ID: "make-changelog", Input: runner.Input{Prompt: "Create CHANGELOG.md with a v0.1.0 heading, then git add and commit it."}},
		{ID: "summary", Input: runner.Input{Prompt: "Reply with exactly: batch done"}, DependsOn: []string{"make-docs", "make-changelog"}},
	}

	fmt.Printf("dispatching %d tasks across 2 workers…\n\n", len(tasks))
	rep, err := f.Run(ctx, tasks)
	if err != nil {
		log.Fatalf("fleet: %v", err)
	}
	fmt.Printf("\n=== %d ok · %d failed · %d skipped · total $%.4f ===\n",
		len(rep.Results)-rep.Failed-rep.Skipped, rep.Failed, rep.Skipped, rep.TotalCostUSD)
}

func oneLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
