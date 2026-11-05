package runner

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Effective is a secret-free description of what one invocation will run with:
// the Runner's defaults merged with the Input's overrides. It is meant for
// viewers and logs (the graph web UI shows it per agent step). Environment
// variables appear by name only, MCP servers by name, type and URL without
// credentials, and argv values that look like secrets are masked.
type Effective struct {
	Prompt         string      `json:"prompt,omitempty"`
	Label          string      `json:"label,omitempty"`
	Model          string      `json:"model"`
	AllowedTools   []string    `json:"allowed_tools,omitempty"`
	MaxTurns       int         `json:"max_turns"`
	MaxBudgetUSD   string      `json:"max_budget_usd,omitempty"`
	TimeoutMS      int64       `json:"timeout_ms"`
	PermissionMode string      `json:"permission_mode,omitempty"`
	SkipPerms      bool        `json:"skip_permissions"`
	WorkDir        string      `json:"work_dir,omitempty"`
	SystemPrompt   string      `json:"system_prompt,omitempty"`
	Resume         string      `json:"resume,omitempty"`
	Continue       bool        `json:"continue,omitempty"`
	ForkSession    bool        `json:"fork_session,omitempty"`
	AddDirs        []string    `json:"add_dirs,omitempty"`
	ContextFiles   []string    `json:"context_files,omitempty"`
	SettingsPath   string      `json:"settings_path,omitempty"`
	MCPServers     []MCPServer `json:"mcp_servers,omitempty"`
	ExtraArgs      []string    `json:"extra_args,omitempty"`
	EnvNames       []string    `json:"env_names,omitempty"`
	Transport      string      `json:"transport,omitempty"`
}

// MCPServer is one configured MCP server, without headers, env or args.
type MCPServer struct {
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`
	URL     string `json:"url,omitempty"`     // userinfo and query stripped
	Command string `json:"command,omitempty"` // executable only
}

// Effective returns the secret-free effective configuration for in. With a
// zero Input it describes the Runner's defaults.
func (r *Runner) Effective(in Input) Effective {
	e := Effective{
		Prompt: in.Prompt, Label: in.Label, Model: r.modelOf(in),
		AllowedTools: append([]string(nil), r.cfg.AllowedTools...),
		MaxTurns:     r.cfg.MaxTurns, MaxBudgetUSD: r.cfg.MaxBudgetUSD,
		TimeoutMS: r.cfg.ProcessTimeout.Milliseconds(), PermissionMode: in.PermissionMode,
		SkipPerms: r.cfg.skipPermissions, WorkDir: in.WorkDir, SystemPrompt: in.SystemPrompt,
		Resume: in.Resume, Continue: in.Continue, ForkSession: in.ForkSession,
		AddDirs: in.AddDirs, ContextFiles: in.ContextFiles, SettingsPath: in.SettingsPath,
		Transport: fmt.Sprintf("%T", r.cfg.Transport),
	}
	if in.MCPConfigPath != "" {
		e.MCPServers = describeMCP(in.MCPConfigPath)
	}
	e.ExtraArgs = redactArgs(in.ExtraArgs)
	seen := map[string]bool{}
	for _, kv := range append(append([]string(nil), r.cfg.Env...), in.Env...) {
		name, _, _ := strings.Cut(kv, "=")
		if name != "" && !seen[name] {
			seen[name] = true
			e.EnvNames = append(e.EnvNames, name)
		}
	}
	sort.Strings(e.EnvNames)
	return e
}

func describeMCP(path string) []MCPServer {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Servers map[string]struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	var out []MCPServer
	for name, s := range f.Servers {
		m := MCPServer{Name: name, Type: s.Type, URL: stripURL(s.URL)}
		if s.Command != "" {
			m.Command = filepath.Base(s.Command)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// stripURL drops credentials and the query string, where tokens usually live.
func stripURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if i := strings.IndexAny(raw, "?#@"); i >= 0 {
			return "(redacted)"
		}
		return raw
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

var secretName = regexp.MustCompile(`(?i)(key|token|secret|passw|auth|credential|bearer|cookie)`)

// redactArgs masks the value of secret-looking flags (--api-key X, --token=X)
// and any URL-ish argument carrying credentials.
func redactArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	maskNext := false
	for i, a := range args {
		switch {
		case maskNext:
			out[i], maskNext = "***", false
		case strings.HasPrefix(a, "-") && strings.Contains(a, "=") && secretName.MatchString(a[:strings.Index(a, "=")]):
			out[i] = a[:strings.Index(a, "=")+1] + "***"
		case strings.HasPrefix(a, "-") && secretName.MatchString(a):
			out[i], maskNext = a, true
		case strings.Contains(a, "://") && (strings.Contains(a, "@") || strings.Contains(a, "?")):
			out[i] = stripURL(a)
		case secretName.MatchString(a) && strings.Contains(a, "="):
			out[i] = a[:strings.Index(a, "=")+1] + "***"
		default:
			out[i] = a
		}
	}
	return out
}
