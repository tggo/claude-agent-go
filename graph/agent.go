package graph

import (
	"context"
	"errors"
	"fmt"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/runner"
)

// ErrAgentResult: the CLI finished but reported an error result (is_error).
var ErrAgentResult = errors.New("graph: claude reported an error result")

type agentConfig struct {
	retry       *runner.RetryPolicy
	progress    runner.ProgressFunc
	allowErrRes bool
	recoverMsg  string
}

// AgentOption configures Agent.
type AgentOption func(*agentConfig)

// WithRetry runs the agent through RunStreamWithRetry with policy p.
func WithRetry(p runner.RetryPolicy) AgentOption { return func(c *agentConfig) { c.retry = &p } }

// WithProgress forwards every stream event of the run to fn.
func WithProgress(fn runner.ProgressFunc) AgentOption {
	return func(c *agentConfig) { c.progress = fn }
}

// WithRecoverPrompt sets the prompt an agent node sends when Recover re-runs
// it and resumes its logged CLI session (--resume). The default is the retry
// policy's ResumePrompt when WithRetry sets one, else DefaultRecoverPrompt.
func WithRecoverPrompt(p string) AgentOption { return func(c *agentConfig) { c.recoverMsg = p } }

// AllowErrorResult hands results with IsError set to apply instead of failing
// the node, so apply can inspect them and decide (for example to route to a
// repair step). The default is to fail the node with ErrAgentResult.
func AllowErrorResult() AgentOption { return func(c *agentConfig) { c.allowErrRes = true } }

// RunAgent runs in through r with the retry, progress and cost accounting that
// Agent uses, for hand-written nodes that need more control (for example ones
// that may Interrupt). The run's cost is added to the current node via AddCost
// whether or not it succeeded, so a custom node cannot forget it. in.Label
// defaults to the node name. Errors are returned as the runner gave them
// (not wrapped), and an error result is returned as (res, nil) unless the
// caller checks res.IsError; use AllowErrorResult-style handling at the call
// site. Only WithRetry and WithProgress apply.
func RunAgent(ctx context.Context, r *runner.Runner, in runner.Input, opts ...AgentOption) (*runner.Result, error) {
	var cfg agentConfig
	for _, o := range opts {
		o(&cfg)
	}
	return runAgent(ctx, r, in, &cfg)
}

func runAgent(ctx context.Context, r *runner.Runner, in runner.Input, cfg *agentConfig) (*runner.Result, error) {
	if in.Label == "" {
		in.Label = NodeName(ctx)
	}
	if sid, ok := RecoveredSession(ctx); ok && in.Resume == "" {
		// The node crashed with this session running: continue it instead of
		// starting over (the work done and the conversation are kept).
		in.Resume, in.Continue, in.ForkSession = sid, false, false
		switch {
		case cfg.recoverMsg != "":
			in.Prompt = cfg.recoverMsg
		case cfg.retry != nil && cfg.retry.ResumePrompt != "":
			in.Prompt = cfg.retry.ResumePrompt
		default:
			in.Prompt = DefaultRecoverPrompt
		}
	}
	var (
		res *runner.Result
		err error
	)
	if sc, ok := ctx.Value(scopeKey{}).(*scope); ok {
		if io, ok := sc.obs.(AgentInputObserver); ok {
			ai := AgentInfo{Input: r.Effective(in)}
			if cfg.retry != nil {
				ai.Retry = &RetryInfo{MaxAttempts: cfg.retry.MaxAttempts, Resume: cfg.retry.Resume}
				if ai.Retry.MaxAttempts <= 0 {
					ai.Retry.MaxAttempts = 3 // runner.RetryPolicy default
				}
			}
			io.AgentInput(ctx, sc.info, ai)
		}
	}
	progress := cfg.progress
	if sc, ok := ctx.Value(scopeKey{}).(*scope); ok && (sc.ck != nil || sc.hook != nil) {
		user := progress
		progress = func(ev claudecli.StreamEvent, n int) {
			sc.noteSession(ctx, ev.SessionID) // logged before anyone reacts to the event
			if user != nil {
				user(ev, n)
			}
			if sc.hook != nil && sc.hook.OnEvent != nil {
				sc.hook.OnEvent(ev, n)
			}
		}
	}
	if sc, ok := ctx.Value(scopeKey{}).(*scope); ok {
		if ao, ok := sc.obs.(AgentObserver); ok {
			user, info := cfg.progress, sc.info
			progress = func(ev claudecli.StreamEvent, n int) {
				if user != nil {
					user(ev, n)
				}
				ao.AgentEvent(ctx, info, ev)
			}
		}
	}
	if cfg.retry != nil {
		res, err = r.RunStreamWithRetry(ctx, in, progress, *cfg.retry)
	} else {
		res, err = r.RunStream(ctx, in, progress)
	}
	if res != nil {
		AddCost(ctx, res.TotalCostUSD)
	}
	return res, err
}

// Agent returns a node that runs Claude Code through r. build derives the
// request from the state (Input.Label defaults to the node name); apply writes
// the result back. apply may be nil when only the side effects matter.
//
// The run's cost is added to the node via AddCost whether or not it succeeded.
// Errors come back wrapped with the node name. On a Go error from the runner,
// apply is not called. On an error result (res.IsError) the node fails with
// ErrAgentResult unless AllowErrorResult is set.
func Agent[S any](r *runner.Runner, build func(s *S) runner.Input, apply func(s *S, res *runner.Result) error, opts ...AgentOption) NodeFunc[S] {
	var cfg agentConfig
	for _, o := range opts {
		o(&cfg)
	}
	return func(ctx context.Context, s *S) error {
		name := NodeName(ctx)
		res, err := runAgent(ctx, r, build(s), &cfg)
		if err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
		if res.IsError && !cfg.allowErrRes {
			return fmt.Errorf("agent %q: %w: %s", name, ErrAgentResult, res.ResultText)
		}
		if apply != nil {
			if err := apply(s, res); err != nil {
				return fmt.Errorf("agent %q: apply: %w", name, err)
			}
		}
		return nil
	}
}
