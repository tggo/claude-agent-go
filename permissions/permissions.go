// Package permissions is a composable, ordered rule policy for the CLI's
// can_use_tool callback: first matching rule wins, anything unmatched is
// denied.
//
//	p := &permissions.Policy{
//	    Rules: []permissions.Rule{
//	        permissions.Deny("Bash(rm *)", "no deleting"),
//	        permissions.Allow("Bash(git status*)"),
//	        permissions.Allow("mcp__pizza__*"),
//	        permissions.Ask("Write", askUser),
//	    },
//	    Remember: true,
//	    Audit:    func(d permissions.Decision) { log.Printf("%s %s: %s", d.Tool, d.Effect, d.Rule) },
//	}
//	client.New(ctx, client.Config{CanUseTool: p.CanUseTool()})
//
// Patterns follow Claude Code's settings syntax: "Tool" or "Tool(argument)".
// The tool part and the argument are globs where '*' matches any run of
// characters and '?' one character. The argument is matched against the tool's
// primary input field (Bash: command; Read/Edit/Write: file_path; WebFetch:
// url; Grep/Glob: pattern; WebSearch: query; otherwise the first of those
// present, else the raw JSON). A trailing ":*" is the legacy prefix form:
// "Bash(git:*)" matches "git" and "git <anything>".
//
// Shell commands are not trusted blindly. This applies to every tool whose
// input is a shell command (Bash and Monitor). An Allow or Ask rule only
// matches when every segment of a compound command ("a && b", "a | b", "a; b")
// matches, and only when the command is made of plain words: outside quotes
// it may hold letters, digits, whitespace and - _ . / : = , @ + % ^ plus the
// separators; double-quoted text may not hold $, `, \ or !. Anything else
// (expansions, globs, redirections, subshells, escapes) falls through to the
// next rule, usually the default deny, so "ls *.go" needs its own rule or an
// Ask. Deny rules match when any segment matches, including commands nested in
// substitutions, subshells and groups, and see each segment normalized: quotes
// and backslashes removed, whitespace collapsed, leading redirections,
// assignments, keywords (time, !, if, then, ...), wrappers (sudo, env, xargs,
// nohup, nice, timeout, command, exec, ...) and a command's directory
// stripped, so "/bin/rm\t-rf x" is seen as "rm -rf x". Allow rules are not an
// OS sandbox. For file tools (Read, Edit,
// Write, ...) an Allow/Ask rule never matches a path containing "..", and a
// Deny rule also sees the lexically cleaned path.
package permissions

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/client"
)

// Effect is the outcome recorded for a decision.
type Effect string

const (
	EffectAllow       Effect = "allow"
	EffectDeny        Effect = "deny"
	EffectAskAllow    Effect = "ask_allow"
	EffectAskDeny     Effect = "ask_deny"
	EffectRemembered  Effect = "remembered" // an earlier Ask answer was reused
	EffectDefaultDeny Effect = "default_deny"
)

// Request is what a rule sees.
type Request struct {
	Tool    string
	Input   json.RawMessage
	Context client.PermissionContext
}

// AskFunc decides an Ask rule: allow, or deny with a message for the model.
// An error denies the call (fail closed).
type AskFunc func(ctx context.Context, req Request) (allow bool, denyMessage string, err error)

type kind int

const (
	kindAllow kind = iota
	kindDeny
	kindAsk
)

// Rule is one entry of a Policy. Build it with Allow, Deny or Ask.
type Rule struct {
	Pattern string
	kind    kind
	message string
	ask     AskFunc
}

// Allow permits calls matching pattern.
func Allow(pattern string) Rule { return Rule{Pattern: pattern, kind: kindAllow} }

// Deny blocks calls matching pattern; msg is shown to the model.
func Deny(pattern, msg string) Rule { return Rule{Pattern: pattern, kind: kindDeny, message: msg} }

// Ask defers calls matching pattern to fn.
func Ask(pattern string, fn AskFunc) Rule { return Rule{Pattern: pattern, kind: kindAsk, ask: fn} }

// Decision is one audited outcome.
type Decision struct {
	Time   time.Time
	Tool   string
	Input  json.RawMessage
	Effect Effect
	// Rule is the matching rule's pattern ("" for the default).
	Rule string
	// Message is the deny message, if any.
	Message string
	// Err is set when an Ask function failed (the call was denied).
	Err error
}

// Allowed reports whether the decision let the call through.
func (d Decision) Allowed() bool {
	return d.Effect == EffectAllow || d.Effect == EffectAskAllow ||
		(d.Effect == EffectRemembered && d.Message == "")
}

// Policy is an ordered rule list. The zero value denies everything. Fields
// must not change after first use; a Policy is safe for concurrent use.
type Policy struct {
	Rules []Rule
	// Remember reuses an Ask answer for the same tool and identical input for
	// the rest of the Policy's life (one session); see Reset. Errors are never
	// remembered.
	Remember bool
	// DefaultMessage is the deny message when no rule matches.
	DefaultMessage string
	// Audit, when set, is called for every decision (possibly concurrently).
	Audit func(Decision)

	mu   sync.Mutex
	memo map[string]memoEntry
}

type memoEntry struct {
	allow bool
	msg   string
	rule  string
}

// Reset forgets remembered Ask answers.
func (p *Policy) Reset() {
	p.mu.Lock()
	p.memo = nil
	p.mu.Unlock()
}

// Decide evaluates the policy for one tool call and audits the outcome.
func (p *Policy) Decide(ctx context.Context, req Request) Decision {
	d := p.decide(ctx, req)
	d.Time, d.Tool, d.Input = time.Now(), req.Tool, req.Input
	if p.Audit != nil {
		p.Audit(d)
	}
	return d
}

func (p *Policy) decide(ctx context.Context, req Request) Decision {
	for _, r := range p.Rules {
		if !r.matches(req) {
			continue
		}
		switch r.kind {
		case kindAllow:
			return Decision{Effect: EffectAllow, Rule: r.Pattern}
		case kindDeny:
			return Decision{Effect: EffectDeny, Rule: r.Pattern, Message: r.denyMsg(req.Tool)}
		}
		return p.ask(ctx, r, req)
	}
	msg := p.DefaultMessage
	if msg == "" {
		msg = fmt.Sprintf("tool %s is not allowed by policy", req.Tool)
	}
	return Decision{Effect: EffectDefaultDeny, Message: msg}
}

func (p *Policy) ask(ctx context.Context, r Rule, req Request) Decision {
	// Length-prefixed so ("a\x00b","c") and ("a","b\x00c") cannot collide.
	key := strconv.Itoa(len(req.Tool)) + ":" + req.Tool + string(req.Input)
	if p.Remember {
		p.mu.Lock()
		m, ok := p.memo[key]
		p.mu.Unlock()
		if ok {
			d := Decision{Effect: EffectRemembered, Rule: m.rule}
			if !m.allow {
				d.Message = m.msg
			}
			return d
		}
	}
	if r.ask == nil {
		return Decision{Effect: EffectAskDeny, Rule: r.Pattern, Message: "no approver configured for " + req.Tool}
	}
	allow, msg, err := r.ask(ctx, req)
	if err != nil {
		return Decision{Effect: EffectAskDeny, Rule: r.Pattern, Message: "permission check failed: " + err.Error(), Err: err}
	}
	if !allow && msg == "" {
		msg = fmt.Sprintf("the user declined %s", req.Tool)
	}
	if p.Remember {
		p.mu.Lock()
		if p.memo == nil {
			p.memo = map[string]memoEntry{}
		}
		p.memo[key] = memoEntry{allow: allow, msg: msg, rule: r.Pattern}
		p.mu.Unlock()
	}
	if allow {
		return Decision{Effect: EffectAskAllow, Rule: r.Pattern}
	}
	return Decision{Effect: EffectAskDeny, Rule: r.Pattern, Message: msg}
}

func (r Rule) denyMsg(tool string) string {
	if r.message != "" {
		return r.message
	}
	return fmt.Sprintf("tool %s is denied by policy", tool)
}

// CanUseTool adapts the policy to client.Config.CanUseTool.
func (p *Policy) CanUseTool() client.CanUseToolFunc {
	return func(ctx context.Context, tool string, input json.RawMessage, pctx client.PermissionContext) (client.PermissionResult, error) {
		d := p.Decide(ctx, Request{Tool: tool, Input: input, Context: pctx})
		if d.Allowed() {
			return client.Allow(), nil
		}
		return client.Deny(d.Message), nil
	}
}

// ---- matching ----

func (r Rule) matches(req Request) bool {
	toolPat, argPat, hasArg := splitPattern(r.Pattern)
	if !glob(toolPat, req.Tool) {
		return false
	}
	if !hasArg {
		return true
	}
	return matchArg(argPat, req.Tool, req.Input, r.kind == kindDeny)
}

// splitPattern splits "Tool(arg)" into its parts.
func splitPattern(p string) (tool, arg string, hasArg bool) {
	i := strings.IndexByte(p, '(')
	if i < 0 || !strings.HasSuffix(p, ")") {
		return p, "", false
	}
	return p[:i], p[i+1 : len(p)-1], true
}

var primaryFields = []string{"command", "file_path", "url", "pattern", "query", "path"}

func primaryInput(tool string, raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		fields := primaryFields
		switch {
		case shellTools[tool]:
			fields = []string{"command"}
		case fileTools[tool]:
			fields = []string{"file_path", "notebook_path"}
		}
		for _, f := range fields {
			if s, ok := m[f].(string); ok {
				return s
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

// shellTools are the tools whose primary input is a shell command or script.
var shellTools = map[string]bool{"Bash": true, "Monitor": true}

func matchArg(pat, tool string, raw json.RawMessage, deny bool) bool {
	in := primaryInput(tool, raw)
	if fileTools[tool] {
		return matchPath(pat, in, deny)
	}
	if !shellTools[tool] {
		return argGlob(pat, in)
	}
	if deny {
		if argGlob(pat, in) {
			return true
		}
		for _, seg := range splitShellDeny(in) {
			if argGlob(pat, seg) || argGlob(pat, normalizeSegment(seg)) {
				return true
			}
		}
		return false
	}
	if !plainShell(in) {
		return false
	}
	segs := splitShell(in)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		if !argGlob(pat, seg) {
			return false
		}
	}
	return true
}

// plainShell reports whether cmd is made only of plain words and separators,
// so no part of it can expand, glob, redirect or evaluate nested code. It is a
// whitelist: anything not provably inert is rejected.
func plainShell(cmd string) bool {
	const quote, dquote = '\'', '"'
	var in rune // 0, quote or dquote
	for _, c := range cmd {
		switch in {
		case quote:
			if c == quote {
				in = 0
			}
			continue
		case dquote:
			switch c {
			case dquote:
				in = 0
			case '$', '`', '\\', '!':
				return false
			}
			continue
		}
		switch {
		case c == quote || c == dquote:
			in = c
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune(" \t-_./:=,@+%^&|;\n\r", c):
		default:
			return false
		}
	}
	return in == 0
}

// shellPrefixes are words that run the rest of the segment as a command:
// keywords, negation and wrappers. Their option words are skipped too.
var shellPrefixes = map[string]bool{
	"!": true, "time": true, "if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "exec": true, "command": true,
	"builtin": true, "nohup": true, "nice": true, "sudo": true, "env": true,
	"xargs": true, "timeout": true, "stdbuf": true, "doas": true, "setsid": true,
	"noglob": true, "nocorrect": true, "coproc": true,
}

// wrapperValueFlags are wrapper options whose value is the next word.
var wrapperValueFlags = map[string]bool{
	"sudo -u": true, "sudo -g": true, "sudo -C": true, "sudo -h": true, "sudo -p": true, "sudo -D": true,
	"doas -u": true, "doas -C": true, "env -u": true, "env -C": true, "env -S": true,
	"nice -n": true, "timeout -s": true, "timeout -k": true,
	"xargs -n": true, "xargs -I": true, "xargs -L": true, "xargs -P": true, "xargs -d": true,
	"xargs -E": true, "xargs -s": true, "xargs -a": true,
}

// normalizeSegment rewrites one shell segment the way the shell will see the
// command word: quotes and backslashes removed, whitespace collapsed, leading
// redirections, NAME=value assignments, keywords and wrappers (with their
// options) dropped, and the command's directory stripped.
func normalizeSegment(seg string) string {
	words := shellWords(seg)
	i := 0
	for i < len(words) {
		w := words[i]
		switch {
		case w == "":
			i++
		case strings.ContainsAny(w[:1], "<>") || (len(w) > 1 && w[0] >= '0' && w[0] <= '9' && strings.ContainsAny(w, "<>")):
			// A bare operator ("> file") consumes the next word as its target.
			if strings.TrimLeft(w, "0123456789<>&|") == "" {
				i++
			}
			i++
		case isAssignment(w):
			i++
		case shellPrefixes[w]:
			wrapper := w
			i++
			for i < len(words) && (strings.HasPrefix(words[i], "-") || isAssignment(words[i]) ||
				(wrapper == "timeout" && strings.Trim(words[i], "0123456789.smhd") == "")) {
				if wrapperValueFlags[wrapper+" "+words[i]] {
					i++ // the flag's value
				}
				i++
			}
		default:
			if j := strings.LastIndexByte(w, '/'); j >= 0 && j < len(w)-1 {
				words[i] = w[j+1:]
			}
			return strings.Join(words[i:], " ")
		}
	}
	return ""
}

func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for k, c := range w[:eq] {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && (k == 0 || !(c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// shellWords splits seg on unquoted whitespace and removes quotes and
// backslash escapes.
func shellWords(seg string) []string {
	var out []string
	var b strings.Builder
	var in rune
	word, esc := false, false
	for _, c := range seg {
		switch {
		case esc:
			b.WriteRune(c)
			esc, word = false, true
		case in != 0:
			if c == in {
				in = 0
			} else {
				b.WriteRune(c)
			}
		case c == '\\':
			esc, word = true, true
		case c == '\'' || c == '"':
			in, word = c, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if word {
				out = append(out, b.String())
				b.Reset()
				word = false
			}
		default:
			b.WriteRune(c)
			word = true
		}
	}
	if word {
		out = append(out, b.String())
	}
	return out
}

// fileTools are the tools whose argument is a filesystem path.
var fileTools = map[string]bool{"Read": true, "Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// matchPath matches a path argument. An Allow/Ask rule never matches a path
// with a ".." element ("/work/../etc/passwd" must not satisfy "/work/*"); a
// Deny rule matches the path as written or lexically resolved.
func matchPath(pat, in string, deny bool) bool {
	slashed := strings.ReplaceAll(in, "\\", "/")
	if deny {
		return argGlob(pat, in) || argGlob(pat, slashed) || argGlob(pat, path.Clean(slashed))
	}
	for _, el := range strings.Split(slashed, "/") {
		if el == ".." {
			return false
		}
	}
	return argGlob(pat, in)
}

// splitShellDeny is splitShell for Deny rules: it additionally cuts at command
// substitution, backticks, subshells and groups so a denied command nested in
// one ("echo $(rm -rf x)", "(rm x)", "{ rm x; }") is still seen.
func splitShellDeny(cmd string) []string {
	return splitShell(strings.Map(func(r rune) rune {
		switch r {
		case '`', '(', ')', '{', '}':
			return ';'
		}
		return r
	}, cmd))
}

// splitShell splits a command on &&, ||, ;, |, & and newlines.
func splitShell(cmd string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	for _, c := range cmd {
		switch c {
		case '&', '|', ';', '\n', '\r':
			flush()
		default:
			b.WriteRune(c)
		}
	}
	flush()
	return out
}

// argGlob matches s against pat, honouring the legacy "prefix:*" form.
func argGlob(pat, s string) bool {
	if prefix, ok := strings.CutSuffix(pat, ":*"); ok {
		return s == prefix || glob(prefix+" *", s)
	}
	return glob(pat, s)
}

// glob matches with '*' (any run) and '?' (one char); everything else literal.
func glob(pat, s string) bool {
	p, t := []rune(pat), []rune(s)
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) && p[pi] != '*':
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
