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
	"math"
	"strconv"
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

	// MaxSpendUSD caps total spend across all tasks. A worker reserves a share
	// of what is left before it runs a task, the task's own budget
	// (--max-budget-usd, through a RetryPolicy MaxSpendUSD) is lowered to that
	// share, and the unspent part is returned when the task ends. Concurrent
	// tasks therefore cannot jointly overrun the cap by a wave of whole task
	// budgets. When in-flight tasks hold the whole remainder, a worker waits
	// for one to finish; when nothing is left, the remaining tasks are skipped.
	// 0 disables the cap.
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
	budgetFreed := sync.NewCond(&mu)
	var spent, reserved float64
	capped := false
	results := make([]TaskResult, 0, len(tasks))

	// emit records a result and, in the same critical section, returns the
	// task's reservation, so no waiter sees the refund before the spend.
	emit := func(tr TaskResult, grant float64) {
		mu.Lock()
		if tr.Result != nil {
			spent += tr.Result.TotalCostUSD
		}
		reserved -= grant
		results = append(results, tr)
		mu.Unlock()
		budgetFreed.Broadcast()
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
				var grant float64
				if f.cfg.MaxSpendUSD > 0 {
					mu.Lock()
					for {
						// Rounded down to 1/100 cent, so a float remainder
						// never becomes a "0" (= unlimited) CLI budget.
						left := math.Floor((f.cfg.MaxSpendUSD-spent-reserved)*1e4) / 1e4
						if left > 0 {
							grant = left
							if b := taskBudget(w); b > 0 && b < grant {
								grant = b
							}
							reserved += grant
							break
						}
						if reserved <= 0 {
							break // spent: nothing in flight can give budget back
						}
						budgetFreed.Wait()
					}
					if grant == 0 {
						capped = true
						mu.Unlock()
						emit(TaskResult{TaskID: task.ID, Worker: w.Name, Skipped: true,
							Err: fmt.Errorf("skipped: fleet spend cap reached")}, 0)
						tl.complete(task.ID, false)
						return
					}
					mu.Unlock()
				}
				res, err := f.runTask(ctx, w, task, grant)
				emit(TaskResult{TaskID: task.ID, Worker: w.Name, Result: res, Err: err}, grant)
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
		emit(TaskResult{TaskID: id, Skipped: true, Err: fmt.Errorf("skipped: %s", reason)}, 0)
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

// taskBudget is the worker's per-invocation budget (--max-budget-usd), 0 when
// none is set.
func taskBudget(w Worker) float64 {
	v, err := strconv.ParseFloat(w.Runner.Effective(runner.Input{}).MaxBudgetUSD, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// runTask runs one task on a worker, in an isolated worktree when configured.
// A grant > 0 is the task's share of the fleet cap: it runs under a retry
// policy whose MaxSpendUSD is at most the grant, which also lowers each
// attempt's --max-budget-usd.
func (f *Fleet) runTask(ctx context.Context, w Worker, t Task, grant float64) (*runner.Result, error) {
	in := t.Input

	if f.cfg.Workspace != nil && f.cfg.Repo != "" {
		wt, _, err := f.cfg.Workspace.CreateWorktree(ctx, f.cfg.Repo, t.ID)
		if err != nil {
			return nil, fmt.Errorf("worktree for task %s: %w", t.ID, err)
		}
		defer f.cfg.Workspace.RemoveWorktree(ctx, f.cfg.Repo, wt)
		in.WorkDir = wt
	}

	if grant > 0 {
		p := runner.RetryPolicy{MaxAttempts: 1}
		if f.cfg.Retry != nil {
			p = *f.cfg.Retry
		}
		if p.MaxSpendUSD <= 0 || p.MaxSpendUSD > grant {
			p.MaxSpendUSD = grant
		}
		return w.Runner.RunJSONWithRetry(ctx, in, p)
	}
	if f.cfg.Retry != nil {
		return w.Runner.RunJSONWithRetry(ctx, in, *f.cfg.Retry)
	}
	return w.Runner.RunJSON(ctx, in)
}
