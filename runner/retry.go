package runner

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// RetryPolicy configures RunJSONWithRetry. The zero value is usable (defaults
// applied): 3 attempts, 1s base / 30s max exponential backoff, no spend cap.
type RetryPolicy struct {
	// MaxAttempts is the total number of tries including the first (default 3).
	MaxAttempts int
	// BaseDelay is the first backoff delay; it doubles each retry (default 1s).
	BaseDelay time.Duration
	// MaxDelay caps a single backoff delay (default 30s).
	MaxDelay time.Duration

	// MaxSpendUSD caps cumulative spend across attempts: once the accumulated
	// cost reaches it, no further retry is made (the last result/error is
	// returned). 0 disables the cap. This is the guard against retries quietly
	// multiplying token spend.
	//
	// The CLI reports cost only in the result event, so an attempt cut off
	// without one (a truncated stream, the usual reason to set Resume) has
	// unknown cost. For the cap such an attempt is charged
	// UnknownAttemptCostUSD, or, when that is 0, the Runner's per-invocation
	// budget (Config.MaxBudgetUSD, default 5.00, the most --max-budget-usd lets
	// one run spend): pessimistic, so a small cap stops resuming after the
	// first truncation rather than never tripping. The estimate affects only
	// this guard; Result.TotalCostUSD and RetryInfo.SpentUSD stay known spend.
	MaxSpendUSD float64

	// UnknownAttemptCostUSD is the cost assumed, for MaxSpendUSD only, for an
	// attempt that ended with no result event. 0 means the Runner's
	// per-invocation budget. See MaxSpendUSD.
	UnknownAttemptCostUSD float64

	// Retryable decides whether a failed attempt should be retried, given its
	// result (may be nil) and error (may be nil for an error-result). nil uses
	// DefaultRetryable.
	Retryable func(res *Result, err error) bool

	// OnRetry, if set, is called before each backoff sleep — for logging/metrics.
	OnRetry func(RetryInfo)

	// Resume makes a retry continue the failed attempt's session (--resume
	// <session_id>) instead of starting over, whenever the failure carries a
	// session id (see SessionIDOf). The work already done — files edited, tool
	// results — is kept, and only the rest is paid for. Used by
	// RunStreamWithRetry; ignored by RunJSONWithRetry (JSON mode reports no
	// session id from a run that died).
	Resume bool

	// ResumePrompt is the prompt sent on a resumed attempt (default
	// DefaultResumePrompt).
	ResumePrompt string
}

// DefaultResumePrompt is sent when resuming a session after a failed attempt.
const DefaultResumePrompt = "Your previous response was interrupted by a connection error. Continue from where you stopped and finish the task."

// RetryInfo is passed to OnRetry before each retry.
type RetryInfo struct {
	Attempt  int           // the attempt that just failed (1-based)
	Err      error         // the Go error, if any
	Result   *Result       // the failed attempt's result, if any
	Delay    time.Duration // wait before the next attempt
	SpentUSD float64       // cumulative spend so far (all attempts)
}

func applyRetryDefaults(p *RetryPolicy) {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 3
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = 30 * time.Second
	}
	if p.Retryable == nil {
		p.Retryable = DefaultRetryable
	}
	if p.ResumePrompt == "" {
		p.ResumePrompt = DefaultResumePrompt
	}
}

// transientMarkers are substrings that indicate a retryable, transient failure.
var transientMarkers = []string{
	"overloaded", "rate limit", "rate_limit", "429", "500", "502", "503",
	"504", "529", "timeout", "timed out", "connection reset", "connection refused",
	"temporarily", "unavailable", "eof", "network",
}

func looksTransient(s string) bool {
	s = strings.ToLower(s)
	for _, m := range transientMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// DefaultRetryable retries timeouts, transient process failures, and error
// results whose api_error_status / text looks transient. It never retries a
// missing binary (CLINotFoundError) or a clean, non-error result.
func DefaultRetryable(res *Result, err error) bool {
	if err != nil {
		if IsCLINotFound(err) {
			return false
		}
		if IsTimeout(err) {
			return true
		}
		if _, ok := IsTruncatedStream(err); ok {
			return true
		}
		if pe, ok := IsProcessError(err); ok {
			return looksTransient(pe.Stderr) || stdoutResultTransient(pe.Stdout)
		}
		return false
	}
	// No Go error: retry only if the CLI reported an error result that looks
	// transient (rate limit / overloaded / 5xx).
	if res != nil && res.IsError {
		blob := res.ResultText
		if res.Metadata != nil {
			blob += " " + string(res.Metadata.APIErrorStatus) + " " + res.Metadata.Subtype
		}
		return looksTransient(blob)
	}
	return false
}

// apiStatusRe finds a transient api_error_status in a stdout tail whose JSON
// line was cut by truncation and so no longer parses.
var apiStatusRe = regexp.MustCompile(`"api_error_status"\s*:\s*"?(429|5\d\d)\b`)

// stdoutResultTransient reports whether the stdout tail of a failed process
// ends in an error result that looks transient. The CLI exits non-zero after
// an is_error result (a 429/529 from the API), leaving stderr empty and the
// reason only in the result event on stdout. Only result events are
// inspected, so assistant text that merely mentions "timeout" never counts.
func stdoutResultTransient(stdout string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		ev := claudecli.ParseStreamLine([]byte(strings.TrimSpace(line)))
		if ev != nil && ev.IsResult() && ev.IsError {
			return looksTransient(ev.Result+" "+ev.Subtype) || apiStatusRe.MatchString(line)
		}
	}
	return apiStatusRe.MatchString(stdout)
}

var retryAfterRe = regexp.MustCompile(`(?i)retry[- ]?after[:\s]+(\d+)`)

// retryAfter extracts a server-suggested delay (seconds) from an attempt's
// stderr/text, or 0 if none.
func retryAfter(res *Result, err error) time.Duration {
	var texts []string
	if pe, ok := IsProcessError(err); ok {
		texts = append(texts, pe.Stderr)
	}
	if res != nil {
		texts = append(texts, res.ResultText)
		if res.Metadata != nil {
			texts = append(texts, string(res.Metadata.APIErrorStatus))
		}
	}
	for _, t := range texts {
		if m := retryAfterRe.FindStringSubmatch(t); m != nil {
			if n, e := strconv.Atoi(m[1]); e == nil {
				return time.Duration(n) * time.Second
			}
		}
	}
	return 0
}

// backoff computes the delay before the next attempt: exponential with ±20%
// jitter, never below a server-suggested retry-after, capped at MaxDelay.
func (p RetryPolicy) backoff(attempt int, res *Result, err error) time.Duration {
	d := float64(p.BaseDelay) * float64(uint(1)<<uint(attempt-1))
	if d > float64(p.MaxDelay) {
		d = float64(p.MaxDelay)
	}
	d *= 0.8 + 0.4*rand.Float64() // ±20% jitter
	delay := time.Duration(d)
	if ra := retryAfter(res, err); ra > delay {
		delay = ra
	}
	if delay > p.MaxDelay && retryAfter(res, err) == 0 {
		delay = p.MaxDelay
	}
	return delay
}

// unknownAttemptCost is the spend assumed for an attempt with no result event.
func (r *Runner) unknownAttemptCost(p RetryPolicy) float64 {
	if p.UnknownAttemptCostUSD > 0 {
		return p.UnknownAttemptCostUSD
	}
	if v, err := strconv.ParseFloat(r.cfg.MaxBudgetUSD, 64); err == nil && v > 0 {
		return v
	}
	return math.MaxFloat64 / 4 // no per-run bound is configured: spend is unbounded
}

// RunJSONWithRetry runs RunJSON with retries on transient failures. It
// accumulates the cost of EVERY attempt (successful, failed, or error-result)
// into the returned Result.TotalCostUSD, sets Result.Attempts, and stops early
// once cumulative spend reaches RetryPolicy.MaxSpendUSD. On exhaustion it
// returns the last result (with cumulative spend) and an error.
func (r *Runner) RunJSONWithRetry(ctx context.Context, in Input, p RetryPolicy) (*Result, error) {
	p.Resume = false
	return r.withRetry(ctx, in, p, r.RunJSON)
}

// RunStreamWithRetry is RunStream with the same retry semantics as
// RunJSONWithRetry, plus truncation handling: a stream cut without a result
// event (TruncatedStreamError) is retryable, and with p.Resume set a retry
// continues the session via --resume instead of rerunning the task. progress
// sees the events of every attempt.
func (r *Runner) RunStreamWithRetry(ctx context.Context, in Input, progress ProgressFunc, p RetryPolicy) (*Result, error) {
	return r.withRetry(ctx, in, p, func(ctx context.Context, in Input) (*Result, error) {
		return r.RunStream(ctx, in, progress)
	})
}

// attemptKey carries the retry attempt number to openRecorder.
type attemptKey struct{}

func (r *Runner) withRetry(ctx context.Context, in Input, p RetryPolicy, run func(context.Context, Input) (*Result, error)) (*Result, error) {
	applyRetryDefaults(&p)

	var spent, guard float64 // known spend; known spend plus estimates for unknown attempts
	var last *Result
	var lastErr error
	attempts := 0

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		attempts = attempt
		res, err := run(context.WithValue(ctx, attemptKey{}, attempt), in)
		if res != nil {
			spent += res.TotalCostUSD
			guard += res.TotalCostUSD
			last = res
		}
		if _, trunc := IsTruncatedStream(err); trunc && (res == nil || res.TotalCostUSD == 0) {
			guard += r.unknownAttemptCost(p)
		}
		lastErr = err

		// Success = no Go error and not an error-result.
		if err == nil && (res == nil || !res.IsError) {
			res.Attempts = attempt
			res.TotalCostUSD = spent
			return res, nil
		}

		if attempt == p.MaxAttempts || !p.Retryable(res, err) {
			break
		}
		// Cost guard: don't spend more once we've hit the cap.
		if p.MaxSpendUSD > 0 && guard >= p.MaxSpendUSD {
			break
		}

		if p.Resume {
			sid := SessionIDOf(err)
			if sid == "" && res != nil {
				sid = res.SessionID
			}
			if sid != "" {
				r.cfg.Logger.Info("claude cli: resuming session after failure", "session_id", sid, "attempt", attempt)
				in.Resume, in.Continue, in.ForkSession = sid, false, false
				in.Prompt = p.ResumePrompt
			}
		}

		delay := p.backoff(attempt, res, err)
		if p.OnRetry != nil {
			p.OnRetry(RetryInfo{Attempt: attempt, Err: err, Result: res, Delay: delay, SpentUSD: spent})
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	if last != nil {
		last.TotalCostUSD = spent
		last.Attempts = attempts
	}
	if lastErr != nil {
		return last, fmt.Errorf("giving up after retries ($%.4f spent): %w", spent, lastErr)
	}
	if last != nil && last.IsError {
		return last, fmt.Errorf("giving up after retries ($%.4f spent): error result: %s", spent, last.ResultText)
	}
	return last, nil
}
