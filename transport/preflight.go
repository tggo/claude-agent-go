package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Preflight errors; test with errors.Is.
var (
	// ErrClaudeNotFound: the claude binary (or the ssh/docker launcher itself)
	// could not be started on the target.
	ErrClaudeNotFound = errors.New("claude CLI not found on target")
	// ErrCLITooOld: claude --version is below the minimum supported version.
	ErrCLITooOld = errors.New("claude CLI too old")
	// ErrNotAuthenticated: the CLI reports no credentials.
	ErrNotAuthenticated = errors.New("claude CLI not authenticated")
)

// MinCLIVersion is the oldest CLI this SDK supports (control-protocol
// can_use_tool is broken before it).
const MinCLIVersion = "2.1.196"

// PreflightOptions tunes Preflight. The zero value checks the binary and
// MinCLIVersion only.
type PreflightOptions struct {
	// MinVersion overrides MinCLIVersion. "none" disables the version gate.
	MinVersion string
	// CheckAuth also verifies credentials (see PreflightInfo.AuthMethod).
	CheckAuth bool
	// Env is the extra environment the real runs will use; an API-key/token
	// variable here counts as credentials.
	Env []string
	// Timeout bounds the whole preflight (default 30s).
	Timeout time.Duration
}

// PreflightInfo is what Preflight learned about the target.
type PreflightInfo struct {
	// Version is the parsed semver ("2.1.318"); Raw is the full --version line.
	Version, Raw string
	// AuthChecked is true when credentials were positively verified; AuthMethod
	// says how ("env" = API key/token variable present in Env or the host
	// environment (Local only), else what `claude auth status` reported, e.g.
	// "claude.ai"). Credentials are never validated against the API, so no
	// tokens are spent — a revoked key still passes.
	AuthChecked bool
	AuthMethod  string
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// credentialEnv are variables that supply credentials without a login.
var credentialEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

// Preflight verifies the target before a run is paid for: it runs
// `claude --version` through t (so ssh/docker reachability and the binary are
// proven), enforces the minimum version, and optionally checks credentials with
// `claude auth status`, which reads local state and costs nothing. On older CLIs
// without `auth status` the credential check is skipped (AuthChecked=false)
// rather than failed. Errors wrap ErrClaudeNotFound / ErrCLITooOld /
// ErrNotAuthenticated.
func Preflight(ctx context.Context, t Transport, opts ...PreflightOptions) (PreflightInfo, error) {
	var o PreflightOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	var info PreflightInfo
	so, se, err := runBounded(ctx, t.Command(ctx, []string{"--version"}, CommandOpts{Env: o.Env}))
	if err != nil {
		return info, startError(err, se)
	}
	info.Raw = strings.TrimSpace(string(so))
	m := versionRe.FindString(info.Raw)
	if m == "" {
		return info, fmt.Errorf("transport: preflight: cannot parse version from %q", info.Raw)
	}
	info.Version = m

	min := o.MinVersion
	if min == "" {
		min = MinCLIVersion
	}
	if min != "none" {
		cmp, perr := CompareVersions(info.Version, min)
		if perr != nil {
			return info, perr
		}
		if cmp < 0 {
			return info, fmt.Errorf("%w: have %s, need >= %s", ErrCLITooOld, info.Version, min)
		}
	}

	if o.CheckAuth {
		if err := checkAuth(ctx, t, o, &info); err != nil {
			return info, err
		}
	}
	return info, nil
}

func startError(err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	var ee *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: %v", ErrClaudeNotFound, err)
	case errors.As(err, &ee) && (ee.ExitCode() == 126 || ee.ExitCode() == 127):
		return fmt.Errorf("%w: %s", ErrClaudeNotFound, msg)
	case errors.As(err, &ee):
		return fmt.Errorf("transport: preflight: claude --version failed: %w: %s", err, msg)
	}
	return fmt.Errorf("transport: preflight: %w", err)
}

func checkAuth(ctx context.Context, t Transport, o PreflightOptions, info *PreflightInfo) error {
	hostEnv := func(k string) string { return "" }
	if _, local := t.(Local); local {
		hostEnv = os.Getenv
	}
	for _, k := range credentialEnv {
		if hostEnv(k) != "" {
			info.AuthChecked, info.AuthMethod = true, "env"
			return nil
		}
		for _, e := range o.Env {
			if v, ok := strings.CutPrefix(e, k+"="); ok && v != "" {
				info.AuthChecked, info.AuthMethod = true, "env"
				return nil
			}
		}
	}
	so, _, _ := runBounded(ctx, t.Command(ctx, []string{"auth", "status"}, CommandOpts{Env: o.Env}))
	var st struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if jerr := json.Unmarshal(so, &st); jerr != nil || st.LoggedIn == nil {
		if ctx.Err() != nil {
			return fmt.Errorf("transport: preflight: auth status: %w", ctx.Err())
		}
		return nil // old CLI / unreadable output: unknown, not a failure
	}
	if !*st.LoggedIn {
		return fmt.Errorf("%w: set ANTHROPIC_API_KEY or run `claude auth login` on the target", ErrNotAuthenticated)
	}
	info.AuthChecked, info.AuthMethod = true, st.AuthMethod
	return nil
}

// CompareVersions compares dotted numeric versions: -1, 0, 1.
func CompareVersions(a, b string) (int, error) {
	pa, pb := versionRe.FindStringSubmatch(a), versionRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return 0, fmt.Errorf("transport: bad version %q or %q", a, b)
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}
