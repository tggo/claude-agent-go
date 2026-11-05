package runner

import (
	"context"

	"github.com/tggo/claude-agent-go/transport"
)

// Preflight errors, re-exported from transport for errors.Is.
var (
	ErrClaudeNotFound   = transport.ErrClaudeNotFound
	ErrCLITooOld        = transport.ErrCLITooOld
	ErrNotAuthenticated = transport.ErrNotAuthenticated
)

// PreflightInfo describes the target; see transport.PreflightInfo.
type PreflightInfo = transport.PreflightInfo

// Preflight checks, through the configured transport and without spending
// tokens, that claude is runnable on the target, that its version is at least
// transport.MinCLIVersion, and that credentials exist: an API-key/token in
// Config.Env (or the host environment for the Local transport), else
// `claude auth status`. It does NOT validate credentials against the API, check
// the model, or check network reach to Anthropic. See transport.Preflight.
func (r *Runner) Preflight(ctx context.Context) (PreflightInfo, error) {
	return r.runPreflight(ctx)
}

func (r *Runner) runPreflight(ctx context.Context) (PreflightInfo, error) {
	return transport.Preflight(ctx, r.cfg.Transport, transport.PreflightOptions{
		CheckAuth: true,
		Env:       r.cfg.Env,
	})
}
