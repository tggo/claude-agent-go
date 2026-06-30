// Command worktree-pr runs an agent in an isolated worktree off a real repo,
// has it implement a small change and commit, then pushes the branch and opens
// a GitHub pull request with the gh CLI.
//
//	# dry run — prints the push + PR commands, changes nothing remote:
//	go run ./examples/worktree-pr -repo /path/to/your/clone
//
//	# actually push the branch and open the PR:
//	go run ./examples/worktree-pr -repo /path/to/your/clone -confirm
//
// Prerequisites: `git`, the `claude` CLI, and (for -confirm) the `gh` CLI
// authenticated with push access to the repo's GitHub `origin`.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

func main() {
	repo := flag.String("repo", "", "path to a local git clone with a GitHub origin (required)")
	base := flag.String("base", "main", "base branch for the PR")
	prompt := flag.String("prompt", "Add a short, friendly note to the top of README.md (or create NOTES.md if there is no README). Keep it to one or two lines.", "task for the agent")
	confirm := flag.Bool("confirm", false, "actually push the branch and open the PR (otherwise dry run)")
	flag.Parse()

	if *repo == "" {
		fmt.Println("usage: go run ./examples/worktree-pr -repo /path/to/clone [-confirm]")
		fmt.Println("  needs a local clone whose `origin` is a GitHub repo you can push to.")
		os.Exit(2)
	}
	repoDir, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ws := workspace.New(filepath.Dir(repoDir), nil)

	// A worktree off the repo, on its own branch. Use a time-free id so the
	// example is deterministic; in real use, pass a task/run id.
	wt, branch, err := ws.CreateWorktree(ctx, repoDir, "agentpr")
	if err != nil {
		log.Fatal(err)
	}
	defer ws.RemoveWorktree(ctx, repoDir, wt)
	fmt.Printf("worktree: %s\nbranch:   %s\n\n", wt, branch)

	// Let the agent implement the change and commit it inside the worktree.
	r := runner.New(
		runner.WithModel("sonnet"),
		runner.WithMaxTurns(12),
		runner.WithTimeout(4*time.Minute),
	)
	res, err := r.Run(ctx, runner.Input{
		WorkDir: wt,
		Prompt:  *prompt + " Then stage your changes and commit them with a clear message.",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("agent: %s\n\n", res.Response)

	// Show what the agent committed.
	diff, _ := ws.RunGit(ctx, wt, "log", "--oneline", *base+".."+branch)
	if diff == "" {
		log.Fatal("the agent made no commit; nothing to open a PR for")
	}
	fmt.Printf("new commits on %s:\n%s\n\n", branch, diff)

	pushCmd := fmt.Sprintf("git -C %s push -u origin %s", wt, branch)
	prCmd := fmt.Sprintf("gh pr create --head %s --base %s --fill", branch, *base)

	if !*confirm {
		fmt.Println("dry run — would now run:")
		fmt.Println("  " + pushCmd)
		fmt.Println("  " + prCmd + "   (in " + wt + ")")
		fmt.Println("\nre-run with -confirm to push the branch and open the PR.")
		return
	}

	// Push the branch.
	if _, err := ws.RunGit(ctx, wt, "push", "-u", "origin", branch); err != nil {
		log.Fatalf("push: %v", err)
	}
	// Open the PR with gh, run from the worktree so it targets the right repo.
	gh := exec.CommandContext(ctx, "gh", "pr", "create", "--head", branch, "--base", *base, "--fill")
	gh.Dir = wt
	gh.Stdout = os.Stdout
	gh.Stderr = os.Stderr
	if err := gh.Run(); err != nil {
		log.Fatalf("gh pr create: %v", err)
	}
	fmt.Println("\n✓ pull request opened.")
}
