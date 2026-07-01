package runner

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
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
	MaxSpendUSD float64

	// Retryable decides whether a failed attempt should be retried, given its
	// result (may be nil) and error (may be nil for an error-result). nil uses
	// DefaultRetryable.
	Retryable func(res *Result, err error) bool

	// OnRetry, if set, is called before each backoff sleep — for logging/metrics.
	OnRetry func(RetryInfo)
}

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
		if pe, ok := IsProcessError(err); ok {
			return looksTransient(pe.Stderr)
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

// RunJSONWithRetry runs RunJSON with retries on transient failures. It
// accumulates the cost of EVERY attempt (successful, failed, or error-result)
// into the returned Result.TotalCostUSD, sets Result.Attempts, and stops early
// once cumulative spend reaches RetryPolicy.MaxSpendUSD. On exhaustion it
// returns the last result (with cumulative spend) and an error.
func (r *Runner) RunJSONWithRetry(ctx context.Context, in Input, p RetryPolicy) (*Result, error) {
	applyRetryDefaults(&p)

	var spent float64
	var last *Result
	var lastErr error

	for attempt := 1; attempt <= p.MaxAttempts; attempt++ {
		res, err := r.RunJSON(ctx, in)
		if res != nil {
			spent += res.TotalCostUSD
			last = res
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
		if p.MaxSpendUSD > 0 && spent >= p.MaxSpendUSD {
			break
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
		last.Attempts = p.MaxAttempts
	}
	if lastErr != nil {
		return last, fmt.Errorf("giving up after retries ($%.4f spent): %w", spent, lastErr)
	}
	if last != nil && last.IsError {
		return last, fmt.Errorf("giving up after retries ($%.4f spent): error result: %s", spent, last.ResultText)
	}
	return last, nil
}
