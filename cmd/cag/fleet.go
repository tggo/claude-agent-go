package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tggo/claude-agent-go/budget"
	"github.com/tggo/claude-agent-go/fleet"
	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

type fleetFile struct {
	Model       string       `yaml:"model"`
	MaxTurns    int          `yaml:"maxTurns"`
	Timeout     string       `yaml:"timeout"`
	MaxSpendUSD float64      `yaml:"maxSpendUSD"`
	Repo        string       `yaml:"repo"`
	Workers     []workerSpec `yaml:"workers"`
	Retry       *retrySpec   `yaml:"retry"`
	Tasks       []taskSpec   `yaml:"tasks"`
}

type workerSpec struct {
	Name          string `yaml:"name"`
	transportSpec `yaml:",inline"`
}

type retrySpec struct {
	MaxAttempts int     `yaml:"maxAttempts"`
	MaxSpendUSD float64 `yaml:"maxSpendUSD"`
}

type taskSpec struct {
	ID        string   `yaml:"id"`
	Prompt    string   `yaml:"prompt"`
	DependsOn []string `yaml:"dependsOn"`
	WorkDir   string   `yaml:"workdir"`
}

func fleetCmd(args []string) {
	fs := flag.NewFlagSet("fleet", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: cag fleet <config.yaml>") }
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}

	raw, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		die("read config: %v", err)
	}
	var cf fleetFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		die("parse config: %v", err)
	}
	if len(cf.Workers) == 0 || len(cf.Tasks) == 0 {
		die("config needs at least one worker and one task")
	}

	timeout := 15 * time.Minute
	if cf.Timeout != "" {
		if d, e := time.ParseDuration(cf.Timeout); e == nil {
			timeout = d
		} else {
			die("bad timeout %q: %v", cf.Timeout, e)
		}
	}
	model := cf.Model
	if model == "" {
		model = "sonnet"
	}

	// Build one runner per worker (same run params, distinct transport).
	var workers []fleet.Worker
	for _, w := range cf.Workers {
		tr, err := w.build()
		if err != nil {
			die("worker %q: %v", w.Name, err)
		}
		opts := []runner.Option{runner.WithModel(model), runner.WithTransport(tr), runner.WithTimeout(timeout), runner.WithLogger(quietLogger())}
		if cf.MaxTurns > 0 {
			opts = append(opts, runner.WithMaxTurns(cf.MaxTurns))
		}
		name := w.Name
		if name == "" {
			name = w.Kind
		}
		workers = append(workers, fleet.Worker{Name: name, Runner: runner.New(opts...)})
	}

	cfg := fleet.Config{Workers: workers, MaxSpendUSD: cf.MaxSpendUSD}
	if cf.Repo != "" {
		cfg.Workspace = workspace.New("", quietLogger())
		cfg.Repo = cf.Repo
	}
	if cf.Retry != nil {
		cfg.Retry = &runner.RetryPolicy{MaxAttempts: cf.Retry.MaxAttempts, MaxSpendUSD: cf.Retry.MaxSpendUSD}
	}

	// A fleet-wide budget just for reporting/warnings on top of the hard cap.
	tracker := budget.New(cf.MaxSpendUSD, budget.WithWarnFraction(0.8),
		budget.OnWarn(func(s budget.Snapshot) {
			fmt.Printf("  ⚠ 80%% of budget used ($%.4f / $%.2f)\n", s.Spent, s.Max)
		}))

	cfg.OnResult = func(tr fleet.TaskResult) {
		switch {
		case tr.Skipped:
			fmt.Printf("  · %-16s SKIPPED (%v)\n", tr.TaskID, tr.Err)
		case tr.Err != nil:
			fmt.Printf("  ✗ %-16s [%s] %v\n", tr.TaskID, tr.Worker, tr.Err)
		default:
			tracker.Add(tr.Result.SessionID, tr.Result.TotalCostUSD)
			fmt.Printf("  ✓ %-16s [%s] $%.4f\n", tr.TaskID, tr.Worker, tr.Result.TotalCostUSD)
		}
	}

	f, err := fleet.New(cfg)
	if err != nil {
		die("%v", err)
	}

	tasks := make([]fleet.Task, len(cf.Tasks))
	for i, ts := range cf.Tasks {
		if ts.ID == "" {
			die("task %d has no id", i)
		}
		tasks[i] = fleet.Task{
			ID:        ts.ID,
			DependsOn: ts.DependsOn,
			Input:     runner.Input{Prompt: ts.Prompt, WorkDir: ts.WorkDir},
		}
	}

	fmt.Printf("dispatching %d tasks across %d workers…\n", len(tasks), len(workers))
	rep, err := f.Run(context.Background(), tasks)
	if err != nil {
		die("fleet: %v", err)
	}
	ok := len(rep.Results) - rep.Failed - rep.Skipped
	fmt.Printf("\n%d ok · %d failed · %d skipped · total $%.4f\n", ok, rep.Failed, rep.Skipped, rep.TotalCostUSD)
	if rep.Failed > 0 {
		os.Exit(1)
	}
}
