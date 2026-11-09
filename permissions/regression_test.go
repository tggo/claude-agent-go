package permissions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func shellReq(tool, cmd string) Request {
	in, _ := json.Marshal(map[string]string{"command": cmd})
	return Request{Tool: tool, Input: in}
}

// Allow(Bash(prefix *)) must not approve shell expansions that run code
// without "$(", backticks or redirections (arithmetic subscripts, zsh glob
// qualifiers, zsh =(...) process substitution).
func TestBashAllowRejectsCodeExpansions(t *testing.T) {
	p := &Policy{Rules: []Rule{Allow("Bash(ls *)")}}
	for _, cmd := range []string{
		`ls ${x:=$'a[\044(touch PWN_ARITH)]'} $[x]`,
		`ls *(e:'touch PWN_GLOBQ':)`,
		`ls =(touch PWN_PROCSUB)`,
		`ls $(id)`,
		`ls $HOME`,
		`ls a\ b`,
		`ls "$(id)"`,
		`ls ~root`,
		`ls {a,b}`,
		`ls !!`,
		`ls 'unterminated`,
	} {
		d := p.Decide(context.Background(), shellReq("Bash", cmd))
		assert.Equal(t, EffectDefaultDeny, d.Effect, cmd)
	}
	for _, cmd := range []string{`ls a b`, `ls -la`, `ls 'a b' "c d"`, `ls 'x$(id)'`, `ls a && ls b`} {
		assert.Equal(t, EffectAllow, p.Decide(context.Background(), shellReq("Bash", cmd)).Effect, cmd)
	}
}

// Deny(Bash(rm *)) must see every spelling the shell runs as rm.
func TestBashDenySeesRespelledCommands(t *testing.T) {
	p := &Policy{Rules: []Rule{Deny("Bash(rm *)", "no"), Allow("Bash")}}
	for _, cmd := range []string{
		"rm -rf x",
		"rm\t-rf x",
		">/dev/null rm -rf x",
		"> /dev/null rm -rf x",
		"2>/dev/null rm -rf x",
		"A=1 rm -rf x",
		"/bin/rm -rf x",
		"r''m -rf x",
		`r\m -rf x`,
		`"rm" -rf x`,
		"time rm x",
		"! rm x",
		"if true; then rm x; fi",
		"xargs rm x",
		"xargs -0 rm x",
		"sudo -u root rm x",
		"env A=1 rm x",
		"nohup nice -n 5 rm x",
		"timeout 5s rm x",
		"command rm x",
	} {
		assert.Equal(t, EffectDeny, p.Decide(context.Background(), shellReq("Bash", cmd)).Effect, cmd)
	}
	for _, cmd := range []string{"ls x", "echo rm", "grep -r rm .", "A=rm ls"} {
		assert.Equal(t, EffectAllow, p.Decide(context.Background(), shellReq("Bash", cmd)).Effect, cmd)
	}
}

// Monitor runs a shell command, so it gets Bash's compound, substitution and
// redirection handling.
func TestMonitorIsShellMatched(t *testing.T) {
	ctx := context.Background()
	allow := &Policy{Rules: []Rule{Allow("Monitor(tail *)")}}
	for _, cmd := range []string{"tail -f app.log; id", "tail -f x $(id)", "tail -f x > /tmp/o", "tail -f x | sh"} {
		assert.Equal(t, EffectDefaultDeny, allow.Decide(ctx, shellReq("Monitor", cmd)).Effect, cmd)
	}
	assert.Equal(t, EffectAllow, allow.Decide(ctx, shellReq("Monitor", "tail -f app.log")).Effect)

	deny := &Policy{Rules: []Rule{Deny("Monitor(rm *)", "no"), Allow("Monitor")}}
	for _, cmd := range []string{"echo hi; rm -rf x", "(rm -rf x)"} {
		assert.Equal(t, EffectDeny, deny.Decide(ctx, shellReq("Monitor", cmd)).Effect, cmd)
	}
	assert.Equal(t, EffectAllow, deny.Decide(ctx, shellReq("Monitor", "tail -f x")).Effect)
}

func TestNormalizeSegmentEdges(t *testing.T) {
	assert.Equal(t, "", normalizeSegment("A=1 >x"))
	assert.Equal(t, "rm x", normalizeSegment(`'/usr/bin/rm' x`))
	assert.Equal(t, "dir/", normalizeSegment("dir/"))
	assert.False(t, isAssignment("1A=x"))
	assert.False(t, isAssignment("=x"))
}
