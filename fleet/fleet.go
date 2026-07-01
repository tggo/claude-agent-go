// Package fleet spreads a batch of agent tasks across a pool of workers, running
// them concurrently. Each worker carries its own runner (and thus its own
// transport — local, docker, or ssh), so one call fans work out across
// machines/containers. Tasks can declare dependencies (a DAG), each task can run
// in an isolated git worktree, and the cost of every run is aggregated with an
// optional fleet-wide spend cap.
//
// This is the orchestration layer for the "remote/distributed Claude Code
// runner" use case: submit tasks, they land on free workers, results come back
// tagged with which worker ran them.
package fleet

import (
	"context"
	"fmt"
	"sync"

	"github.com/tggo/claude-agent-go/runner"
	"github.com/tggo/claude-agent-go/workspace"
)

// Worker is one execution target: a named runner. Two workers with docker/ssh
// transports let the fleet run across hosts; two local workers give local
// parallelism.
type Worker struct {
	Name   string
	Runner *runner.Runner
}

// Task is one unit of work. DependsOn lists task IDs that must succeed first.
type Task struct {
	ID        string
	Input     runner.Input
	DependsOn []string
}

// TaskResult is the outcome of one task, tagged with the worker that ran it.
type TaskResult struct {
	TaskID  string
	Worker  string
	Result  *runner.Result // nil if the task errored before producing one
	Err     error          // non-nil on failure; a special skipped error if never run
	Skipped bool           // true when a failed dependency / cost cap prevented running
}

// Config configures a Fleet.
type Config struct {
	// Workers is the pool; concurrency equals its length. At least one required.
	Workers []Worker

	// MaxSpendUSD is a soft cap on total spend across all tasks: once cumulative
	// spend reaches it, workers stop claiming new tasks. It is best-effort —
	// tasks already in flight (up to len(Workers)) still complete, so actual
	// spend may overshoot by up to one wave. 0 disables the cap.
	MaxSpendUSD float64

	// Retry, when set, runs each task via RunJSONWithRetry with this policy.
	Retry *runner.RetryPolicy

	// Workspace + Repo, when both set, run each task in its own git worktree off
	// Repo (isolated checkout + branch), removed when the task finishes.
	Workspace *workspace.Workspace
	Repo      string

	// OnResult is called as each task completes (the tagged result stream).
	OnResult func(TaskResult)
}

// Report is the aggregate outcome of a Run.
type Report struct {
	Results      []TaskResult
	TotalCostUSD float64
	Failed       int
	Skipped      int
}

// Fleet runs task batches across its workers.
type Fleet struct {
	cfg Config
}

// New validates the config and returns a Fleet.
func New(cfg Config) (*Fleet, error) {
	if len(cfg.Workers) == 0 {
		return nil, fmt.Errorf("fleet: at least one worker required")
	}
	for i, w := range cfg.Workers {
		if w.Runner == nil {
			return nil, fmt.Errorf("fleet: worker %d (%q) has nil Runner", i, w.Name)
		}
	}
	if (cfg.Workspace == nil) != (cfg.Repo == "") {
		return nil, fmt.Errorf("fleet: Workspace and Repo must be set together")
	}
	return &Fleet{cfg: cfg}, nil
}

// Run executes the task DAG. It blocks until every task has run, been skipped,
// or the context is cancelled. The returned Report always reflects what ran.
func (f *Fleet) Run(ctx context.Context, tasks []Task) (*Report, error) {
	tl := newTaskList(tasks)

	var mu sync.Mutex
	var spent float64
	capped := false
	results := make([]TaskResult, 0, len(tasks))

	emit := func(tr TaskResult) {
		mu.Lock()
		if tr.Result != nil {
			spent += tr.Result.TotalCostUSD
		}
		results = append(results, tr)
		mu.Unlock()
		if f.cfg.OnResult != nil {
			f.cfg.OnResult(tr)
		}
	}

	var wg sync.WaitGroup
	for _, w := range f.cfg.Workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				if f.cfg.MaxSpendUSD > 0 && spent >= f.cfg.MaxSpendUSD {
					capped = true
					mu.Unlock()
					return
				}
				mu.Unlock()

				task, ok := tl.claim()
				if !ok {
					return
				}
				res, err := f.runTask(ctx, w, task)
				emit(TaskResult{TaskID: task.ID, Worker: w.Name, Result: res, Err: err})
				tl.complete(task.ID, err == nil)
			}
		}(w)
	}
	wg.Wait()

	// Any task never claimed (blocked by a failed/uncompleted dependency, a
	// dependency cycle, cost cap, or cancellation) is reported as skipped.
	for _, id := range tl.unfinished() {
		reason := "dependency not satisfied"
		if capped {
			reason = "fleet spend cap reached"
		}
		emit(TaskResult{TaskID: id, Skipped: true, Err: fmt.Errorf("skipped: %s", reason)})
	}

	rep := &Report{Results: results, TotalCostUSD: spent}
	for _, r := range results {
		if r.Skipped {
			rep.Skipped++
		} else if r.Err != nil {
			rep.Failed++
		}
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	return rep, nil
}

// runTask runs one task on a worker, in an isolated worktree when configured.
func (f *Fleet) runTask(ctx context.Context, w Worker, t Task) (*runner.Result, error) {
	in := t.Input

	if f.cfg.Workspace != nil && f.cfg.Repo != "" {
		wt, _, err := f.cfg.Workspace.CreateWorktree(ctx, f.cfg.Repo, t.ID)
		if err != nil {
			return nil, fmt.Errorf("worktree for task %s: %w", t.ID, err)
		}
		defer f.cfg.Workspace.RemoveWorktree(ctx, f.cfg.Repo, wt)
		in.WorkDir = wt
	}

	if f.cfg.Retry != nil {
		return w.Runner.RunJSONWithRetry(ctx, in, *f.cfg.Retry)
	}
	return w.Runner.RunJSON(ctx, in)
}
