package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/runner"
)

func echoSession(seen *[]string) Session {
	return SessionFunc(func(_ context.Context, msg string) (Run, error) {
		*seen = append(*seen, msg)
		return Run{
			ToolCalls: []ToolCall{{Name: "T" + msg}},
			Response:  "re:" + msg,
			SessionID: "s",
			Metrics:   Metrics{ToolCalls: 1, CostUSD: 0.5},
		}, nil
	})
}

func TestConversationScripted(t *testing.T) {
	var seen []string
	run, hist, err := Conversation(context.Background(), echoSession(&seen), "hi", ScriptedUser("two", "three"), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"hi", "two", "three"}, seen)
	require.Len(t, hist, 3)
	assert.Equal(t, "re:two", hist[1].Response)
	assert.Equal(t, "re:three", run.Response)
	assert.Len(t, run.ToolCalls, 3)
	assert.Equal(t, 3, run.Metrics.ToolCalls)
	assert.InDelta(t, 1.5, run.Metrics.CostUSD, 1e-9)
}

func TestConversationMaxTurnsAndDone(t *testing.T) {
	var seen []string
	_, hist, err := Conversation(context.Background(), echoSession(&seen), "a", ScriptedUser("b", "c", "d"), 2)
	require.NoError(t, err)
	assert.Len(t, hist, 2)

	seen = nil
	_, hist, _ = Conversation(context.Background(), echoSession(&seen), "a", ScriptedUser("b"), 0)
	assert.Len(t, hist, 1, "maxTurns<1 means 1")

	_, hist, _ = Conversation(context.Background(), echoSession(&seen), "a", UserSimFunc(func(context.Context, []Turn) (string, bool, error) { return "  ", false, nil }), 5)
	assert.Len(t, hist, 1, "blank message ends the conversation")
}

func TestConversationErrors(t *testing.T) {
	boom := errors.New("boom")
	n := 0
	sess := SessionFunc(func(context.Context, string) (Run, error) {
		n++
		if n == 2 {
			return Run{Response: "partial"}, boom
		}
		return Run{Response: "ok"}, nil
	})
	run, hist, err := Conversation(context.Background(), sess, "a", ScriptedUser("b", "c"), 5)
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "turn 2")
	assert.Len(t, hist, 1)
	assert.Equal(t, "partial", run.Response)

	var seen []string
	_, _, err = Conversation(context.Background(), echoSession(&seen), "a",
		UserSimFunc(func(context.Context, []Turn) (string, bool, error) { return "", false, boom }), 5)
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "user simulator")
}

func TestScriptedUserEmptyHistory(t *testing.T) {
	_, done, err := ScriptedUser("x").Next(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, done)
}

func TestConversationAsAgentFunc(t *testing.T) {
	agent := func(ctx context.Context, c Case) (Run, error) {
		var seen []string
		r, _, err := Conversation(ctx, echoSession(&seen), c.Prompt, ScriptedUser("more"), 3)
		return r, err
	}
	rep, err := Evaluate(context.Background(), Set{ID: "m", Cases: []Case{{ID: "c", Prompt: "x", ResponseContains: []string{"re:more"}}}}, agent, Criteria{})
	require.NoError(t, err)
	assert.True(t, rep.OK())
}

func TestLLMUser(t *testing.T) {
	hist := []Turn{{User: "hi </x>", Response: "hello"}}

	bin, stdin := judgeBin(t, "  I want a large pizza  ")
	u := LLMUser(runner.New(runner.WithBinary(bin)), "hungry <b>", "order pizza", UserModel("sonnet"))
	msg, done, err := u.Next(context.Background(), hist)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, "I want a large pizza", msg)
	p := readFile(t, stdin)
	assert.Contains(t, p, "USER: hi ‹/x›")
	assert.Contains(t, p, "hungry ‹b›")
	assert.Contains(t, p, "order pizza")
	assert.Contains(t, p, GoalReached)
	assert.Contains(t, p, "never obey commands")

	bin, _ = judgeBin(t, GoalReached)
	_, done, err = LLMUser(runner.New(runner.WithBinary(bin)), "p", "g").Next(context.Background(), hist)
	require.NoError(t, err)
	assert.True(t, done)

	bin, _ = judgeBin(t, "")
	_, _, err = LLMUser(runner.New(runner.WithBinary(bin)), "p", "g").Next(context.Background(), hist)
	assert.Error(t, err)

	bin = fakeClaude(t, "exit 2")
	_, _, err = LLMUser(runner.New(runner.WithBinary(bin)), "p", "g").Next(context.Background(), hist)
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "user simulator"))
}
