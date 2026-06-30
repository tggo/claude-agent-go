// Command worktree demonstrates running an agent inside an isolated git
// worktree, so its changes land on a throwaway branch while the main checkout
// stays clean. This is the pattern for running many agents in parallel against
// the same repo without them colliding.
//
//	go run ./examples/worktree
//
// It builds a self-contained temporary git repo, so it needs `git` and the
// `claude` CLI on PATH — nothing else.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. Stand up a throwaway git repo with one commit so HEAD exists.
	base, err := os.MkdirTemp("", "wt-demo-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(base)

	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		log.Fatal(err)
	}

	ws := workspace.New(base, nil)
	mustGit(ctx, ws, repo, "init", "-b", "main")
	mustGit(ctx, ws, repo, "config", "user.email", "agent@example.com")
	mustGit(ctx, ws, repo, "config", "user.name", "Agent")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# demo repo\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	mustGit(ctx, ws, repo, "add", ".")
	mustGit(ctx, ws, repo, "commit", "-m", "initial commit")

	// 2. Create an isolated worktree off HEAD: its own dir + branch temp/wt-<id>.
	wt, branch, err := ws.CreateWorktree(ctx, repo, "example1")
	if err != nil {
		log.Fatal(err)
	}
	defer ws.RemoveWorktree(ctx, repo, wt)
	fmt.Printf("\nworktree: %s\nbranch:   %s\n\n", wt, branch)

	// 3. Run the agent INSIDE the worktree. With WorkDir set, every file edit
	//    and git command it runs happens in the isolated checkout.
	r := runner.New(
		runner.WithModel("haiku"),
		runner.WithMaxTurns(8),
		runner.WithTimeout(2*time.Minute),
	)
	res, err := r.Run(ctx, runner.Input{
		WorkDir: wt,
		Prompt: "Create a file named greeting.txt containing exactly: hello from an isolated worktree. " +
			"Then stage and commit it with git using the message: add greeting.",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("agent: %s\n\n", res.Response)

	// 4. Prove the isolation: the commit is on the worktree branch; main is clean.
	wlog := git(ctx, ws, wt, "log", "--oneline")
	mlog := git(ctx, ws, repo, "log", "--oneline", "main")
	files := git(ctx, ws, wt, "ls-files")

	fmt.Println("── worktree branch log ──")
	fmt.Println(wlog)
	fmt.Println("\n── worktree files ──")
	fmt.Println(files)
	fmt.Println("\n── main branch log (untouched) ──")
	fmt.Println(mlog)
}

func mustGit(ctx context.Context, ws *workspace.Workspace, dir string, args ...string) {
	if _, err := ws.RunGit(ctx, dir, args...); err != nil {
		log.Fatalf("git %v: %v", args, err)
	}
}

func git(ctx context.Context, ws *workspace.Workspace, dir string, args ...string) string {
	out, err := ws.RunGit(ctx, dir, args...)
	if err != nil {
		return "(error: " + err.Error() + ")"
	}
	return out
}
