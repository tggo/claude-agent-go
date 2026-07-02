package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var (
		model    = fs.String("model", "sonnet", "model alias or id")
		ts       transportSpec
		workDir  = fs.String("workdir", "", "working directory for the run")
		maxTurns = fs.Int("max-turns", 50, "max agentic turns")
		timeout  = fs.Duration("timeout", 15*time.Minute, "per-run timeout")
		tools    = fs.String("allowed-tools", "", "comma-separated tool allowlist")
		sys      = fs.String("system-prompt", "", "system prompt")
		mcp      = fs.String("mcp-config", "", "path to an MCP config file")
		resume   = fs.String("resume", "", "resume a session id")
		stream   = fs.Bool("stream", false, "stream events live (else JSON result)")
		budget   = fs.String("max-budget", "", "per-run spend cap in USD (e.g. 0.50)")
		retry    = fs.Bool("retry", false, "retry transient failures")
		attempts = fs.Int("retry-attempts", 3, "max attempts with --retry")
		retrySp  = fs.Float64("retry-max-spend", 0, "cap cumulative retry spend (USD; 0=off)")
	)
	fs.StringVar(&ts.Kind, "transport", "local", "local | ssh | docker | docker-run")
	fs.StringVar(&ts.Binary, "bin", "claude", "claude binary path on the target")
	fs.StringVar(&ts.Host, "host", "", "ssh host (user@host)")
	fs.StringVar(&ts.Container, "container", "", "container name (docker)")
	fs.StringVar(&ts.Image, "image", "", "image (docker-run)")
	fs.StringVar(&ts.Network, "network", "", "docker-run --network")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: cag run [flags] "<prompt>"`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		// allow piping the prompt on stdin
		if b, _ := io.ReadAll(os.Stdin); len(strings.TrimSpace(string(b))) > 0 {
			prompt = string(b)
		}
	}
	if strings.TrimSpace(prompt) == "" {
		die("a prompt is required (argument or stdin)")
	}

	tr, err := ts.build()
	if err != nil {
		die("%v", err)
	}

	opts := []runner.Option{
		runner.WithModel(*model),
		runner.WithTransport(tr),
		runner.WithMaxTurns(*maxTurns),
		runner.WithTimeout(*timeout),
		runner.WithLogger(quietLogger()),
	}
	if *budget != "" {
		opts = append(opts, runner.WithMaxBudgetUSD(*budget))
	}
	if *tools != "" {
		opts = append(opts, runner.WithAllowedTools(strings.Split(*tools, ",")...))
	}
	r := runner.New(opts...)

	in := runner.Input{Prompt: prompt, WorkDir: *workDir, SystemPrompt: *sys, MCPConfigPath: *mcp, Resume: *resume}
	ctx := context.Background()

	if *stream {
		res, err := r.RunStream(ctx, in, func(ev claudecli.StreamEvent, _ int) {
			if t := ev.AssistantText(); t != "" {
				fmt.Print(t)
			}
		})
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("\n\n--- cost $%.5f · turns %d · %s\n", res.TotalCostUSD, res.NumTurns, res.Duration.Round(time.Millisecond))
		return
	}

	var res *runner.Result
	if *retry {
		res, err = r.RunJSONWithRetry(ctx, in, runner.RetryPolicy{MaxAttempts: *attempts, MaxSpendUSD: *retrySp})
	} else {
		res, err = r.RunJSON(ctx, in)
	}
	if err != nil {
		die("%v", err)
	}
	fmt.Println(res.ResultText)
	fmt.Printf("\n--- session %s · cost $%.5f · turns %d · attempts %d\n",
		res.SessionID, res.TotalCostUSD, res.NumTurns, max1(res.Attempts))
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
