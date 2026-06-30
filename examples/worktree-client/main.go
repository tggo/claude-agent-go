// Command worktree-client runs an interactive, multi-turn session inside an
// isolated git worktree. The session keeps memory across turns, and every edit
// it makes lands on the worktree's throwaway branch — not the main checkout.
//
//	go run ./examples/worktree-client
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

	"github.com/tggo/claude-agent-go/client"
	"github.com/tggo/claude-agent-go/workspace"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	base, err := os.MkdirTemp("", "wt-client-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(base)

	quiet := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.New(base, quiet)

	repo := filepath.Join(base, "repo")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "agent@example.com"},
		{"config", "user.name", "Agent"},
	} {
		if _, err := ws.RunGit(ctx, repo, args...); err != nil {
			log.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("# demo\n"), 0o644)
	ws.RunGit(ctx, repo, "add", ".")
	ws.RunGit(ctx, repo, "commit", "-m", "initial commit")

	wt, branch, err := ws.CreateWorktree(ctx, repo, "session1")
	if err != nil {
		log.Fatal(err)
	}
	defer ws.RemoveWorktree(ctx, repo, wt)
	fmt.Printf("worktree: %s\nbranch:   %s\n\n", wt, branch)

	// One process, pointed at the worktree, across several turns.
	c, err := client.New(ctx, client.Config{
		Model:    "haiku",
		WorkDir:  wt,
		MaxTurns: 8,
		Logger:   quiet,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	turns := []string{
		"Create a file notes.txt with a single line: first note.",
		// Relies on memory of what notes.txt is from turn 1.
		"Append a second line to that same file: second note.",
		"Now stage notes.txt and commit it with the message: add notes.",
	}
	for i, prompt := range turns {
		t, err := c.Query(ctx, prompt, nil)
		if err != nil {
			log.Fatalf("turn %d: %v", i+1, err)
		}
		fmt.Printf("turn %d → %s\n", i+1, firstLine(t.Text))
	}

	fmt.Println("\n── worktree branch log ──")
	wlog, _ := ws.RunGit(ctx, wt, "log", "--oneline")
	fmt.Println(wlog)
	fmt.Println("── notes.txt (built across turns) ──")
	if b, err := os.ReadFile(filepath.Join(wt, "notes.txt")); err == nil {
		fmt.Print(string(b))
	}
	mlog, _ := ws.RunGit(ctx, repo, "log", "--oneline", "main")
	fmt.Printf("\nmain branch (untouched): %s\n", mlog)
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
