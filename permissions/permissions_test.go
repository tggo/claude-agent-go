package permissions

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/client"
)

func bash(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

func TestGlob(t *testing.T) {
	for _, tt := range []struct {
		pat, s string
		want   bool
	}{
		{"Bash", "Bash", true}, {"Bash", "bash", false}, {"mcp__pizza__*", "mcp__pizza__order", true},
		{"mcp__pizza__*", "mcp__other__order", false}, {"*", "", true}, {"a?c", "abc", true}, {"a?c", "ac", false},
		{"a*b*c", "aXXbYYc", true}, {"a*b*c", "aXXbYY", false}, {"*b", "abab", true}, {"", "", true}, {"", "x", false},
		{"é*", "éa", true},
	} {
		assert.Equal(t, tt.want, glob(tt.pat, tt.s), "%q ~ %q", tt.pat, tt.s)
	}
}

func TestPatternMatching(t *testing.T) {
	read, _ := json.Marshal(map[string]string{"file_path": "/etc/passwd"})
	fetch, _ := json.Marshal(map[string]string{"url": "https://x.dev/a"})
	odd, _ := json.Marshal(map[string]any{"zzz": 1})
	for _, tt := range []struct {
		name string
		rule Rule
		tool string
		in   json.RawMessage
		want bool
	}{
		{"tool only", Allow("Bash"), "Bash", bash("ls"), true},
		{"tool mismatch", Allow("Bash"), "Read", read, false},
		{"prefix glob", Allow("Bash(git status*)"), "Bash", bash("git status -s"), true},
		{"prefix glob no match", Allow("Bash(git status*)"), "Bash", bash("git push"), false},
		{"legacy prefix", Allow("Bash(git:*)"), "Bash", bash("git log"), true},
		{"legacy prefix bare", Allow("Bash(git:*)"), "Bash", bash("git"), true},
		{"legacy prefix not word", Allow("Bash(git:*)"), "Bash", bash("gitk"), false},
		{"compound all ok", Allow("Bash(git *)"), "Bash", bash("git add . && git commit"), true},
		{"compound one bad", Allow("Bash(git status*)"), "Bash", bash("git status && rm -rf /"), false},
		{"pipe bad", Allow("Bash(git status*)"), "Bash", bash("git status | sh"), false},
		{"semicolon bad", Allow("Bash(ls*)"), "Bash", bash("ls; curl evil"), false},
		{"newline bad", Allow("Bash(ls*)"), "Bash", bash("ls\ncurl evil"), false},
		{"subst bad", Allow("Bash(echo *)"), "Bash", bash("echo $(cat /etc/passwd)"), false},
		{"backtick bad", Allow("Bash(echo *)"), "Bash", bash("echo `id`"), false},
		{"redirect bad", Allow("Bash(echo *)"), "Bash", bash("echo hi > /etc/x"), false},
		{"empty command", Allow("Bash(*)"), "Bash", bash(" ; "), false},
		{"deny any segment", Deny("Bash(rm *)", ""), "Bash", bash("ls && rm -rf /"), true},
		{"deny whole", Deny("Bash(rm *)", ""), "Bash", bash("rm x"), true},
		{"deny miss", Deny("Bash(rm *)", ""), "Bash", bash("ls"), false},
		{"read path", Allow("Read(/etc/*)"), "Read", read, true},
		{"read path miss", Allow("Read(/tmp/*)"), "Read", read, false},
		{"url", Allow("WebFetch(https://x.dev/*)"), "WebFetch", fetch, true},
		{"fallback raw json", Allow("Custom(*zzz*)"), "Custom", odd, true},
		{"malformed pattern is a literal tool", Allow("Bash("), "Bash", bash("ls"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.rule.matches(Request{Tool: tt.tool, Input: tt.in}))
		})
	}
}

func TestFirstMatchWinsAndDefaultDeny(t *testing.T) {
	var audit []Decision
	p := &Policy{
		Rules: []Rule{Deny("Bash(git push*)", "no pushing"), Allow("Bash(git *)"), Allow("mcp__pizza__*")},
		Audit: func(d Decision) { audit = append(audit, d) },
	}
	ctx := context.Background()

	d := p.Decide(ctx, Request{Tool: "Bash", Input: bash("git push origin")})
	assert.Equal(t, EffectDeny, d.Effect)
	assert.Equal(t, "no pushing", d.Message)
	assert.False(t, d.Allowed())

	d = p.Decide(ctx, Request{Tool: "Bash", Input: bash("git status")})
	assert.True(t, d.Allowed())
	assert.Equal(t, "Bash(git *)", d.Rule)

	assert.True(t, p.Decide(ctx, Request{Tool: "mcp__pizza__order"}).Allowed())

	d = p.Decide(ctx, Request{Tool: "Write"})
	assert.Equal(t, EffectDefaultDeny, d.Effect)
	assert.Contains(t, d.Message, "Write")

	p.DefaultMessage = "nope"
	assert.Equal(t, "nope", p.Decide(ctx, Request{Tool: "Write"}).Message)

	assert.Len(t, audit, 5)
	assert.Equal(t, "Write", audit[3].Tool)
	assert.False(t, audit[0].Time.IsZero())

	assert.Equal(t, "tool X is denied by policy", Deny("X", "").denyMsg("X"))
	assert.False(t, (&Policy{}).Decide(ctx, Request{Tool: "Bash"}).Allowed(), "zero value denies")
}

func TestAskAndRemember(t *testing.T) {
	var calls atomic.Int32
	answer := true
	ask := func(context.Context, Request) (bool, string, error) {
		calls.Add(1)
		return answer, "", nil
	}
	p := &Policy{Rules: []Rule{Ask("Write", ask)}, Remember: true}
	ctx := context.Background()
	in := json.RawMessage(`{"file_path":"/a"}`)

	d := p.Decide(ctx, Request{Tool: "Write", Input: in})
	assert.Equal(t, EffectAskAllow, d.Effect)
	d = p.Decide(ctx, Request{Tool: "Write", Input: in})
	assert.Equal(t, EffectRemembered, d.Effect)
	assert.True(t, d.Allowed())
	assert.Equal(t, int32(1), calls.Load())

	// different input asks again; a denial is remembered too
	answer = false
	d = p.Decide(ctx, Request{Tool: "Write", Input: json.RawMessage(`{"file_path":"/b"}`)})
	assert.Equal(t, EffectAskDeny, d.Effect)
	assert.Contains(t, d.Message, "declined")
	d = p.Decide(ctx, Request{Tool: "Write", Input: json.RawMessage(`{"file_path":"/b"}`)})
	assert.Equal(t, EffectRemembered, d.Effect)
	assert.False(t, d.Allowed())
	assert.Equal(t, int32(2), calls.Load())

	p.Reset()
	p.Decide(ctx, Request{Tool: "Write", Input: in})
	assert.Equal(t, int32(3), calls.Load())

	// without Remember, always asks
	q := &Policy{Rules: []Rule{Ask("Write", ask)}}
	q.Decide(ctx, Request{Tool: "Write"})
	q.Decide(ctx, Request{Tool: "Write"})
	assert.Equal(t, int32(5), calls.Load())
}

func TestAskErrorsFailClosedAndAreNotRemembered(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	p := &Policy{Remember: true, Rules: []Rule{Ask("X", func(context.Context, Request) (bool, string, error) {
		n++
		return true, "", boom
	})}}
	d := p.Decide(context.Background(), Request{Tool: "X"})
	assert.False(t, d.Allowed())
	assert.ErrorIs(t, d.Err, boom)
	p.Decide(context.Background(), Request{Tool: "X"})
	assert.Equal(t, 2, n)

	nilAsk := &Policy{Rules: []Rule{Ask("X", nil)}}
	assert.False(t, nilAsk.Decide(context.Background(), Request{Tool: "X"}).Allowed())
}

func TestCanUseToolAdapter(t *testing.T) {
	p := &Policy{Rules: []Rule{Allow("Read"), Deny("Bash", "no shell")}}
	var f client.CanUseToolFunc = p.CanUseTool()

	res, err := f(context.Background(), "Read", nil, client.PermissionContext{})
	require.NoError(t, err)
	assert.Equal(t, client.Allow(), res)

	res, err = f(context.Background(), "Bash", bash("ls"), client.PermissionContext{ToolUseID: "t1"})
	require.NoError(t, err)
	assert.Equal(t, client.Deny("no shell"), res)
}

func TestConcurrentUse(t *testing.T) {
	p := &Policy{Remember: true, Rules: []Rule{Ask("*", func(context.Context, Request) (bool, string, error) { return true, "", nil })},
		Audit: func(Decision) {}}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Decide(context.Background(), Request{Tool: "T", Input: json.RawMessage(`{"i":` + string(rune('0'+i%5)) + `}`)})
		}()
	}
	wg.Wait()
}

// Regression (found by fuzzing): ".." let a path escape an Allow glob.
func TestFileRulesRejectDotDot(t *testing.T) {
	p := &Policy{Rules: []Rule{Deny("Read(/etc/*)", "no"), Allow("Read(/work/*)")}}
	read := func(path string) Decision {
		b, _ := json.Marshal(map[string]string{"file_path": path})
		return p.Decide(context.Background(), Request{Tool: "Read", Input: b})
	}
	assert.True(t, read("/work/a.go").Allowed())
	assert.False(t, read("/work/../etc/passwd").Allowed())
	assert.Equal(t, EffectDeny, read("/work/../etc/passwd").Effect, "deny sees the cleaned path")
	assert.False(t, read(`/work/..\secret`).Allowed())
}

// Regression (found by fuzzing): a Deny on a command must see it nested in
// substitutions, subshells and groups, not only in top-level segments.
func TestBashDenySeesNestedCommands(t *testing.T) {
	p := &Policy{Rules: []Rule{Deny("Bash(rm *)", "no"), Allow("Bash")}}
	for _, cmd := range []string{"echo $(rm -rf x)", "echo `rm -rf x`", "(rm -rf x)", "{ rm -rf x; }", "ls; (rm x)"} {
		assert.False(t, p.Decide(context.Background(), Request{Tool: "Bash", Input: bash(cmd)}).Allowed(), cmd)
	}
	assert.True(t, p.Decide(context.Background(), Request{Tool: "Bash", Input: bash("echo $(date)")}).Allowed())
}

// Regression (found by fuzzing): the Remember key must not collide when the
// tool name contains the separator byte.
func TestRememberKeyNoCollision(t *testing.T) {
	ask := func(_ context.Context, r Request) (bool, string, error) { return r.Tool == "a\x00b", "", nil }
	p := &Policy{Rules: []Rule{Ask("*", ask)}, Remember: true}
	assert.True(t, p.Decide(context.Background(), Request{Tool: "a\x00b", Input: json.RawMessage("c")}).Allowed())
	assert.False(t, p.Decide(context.Background(), Request{Tool: "a", Input: json.RawMessage("b\x00c")}).Allowed())
}
