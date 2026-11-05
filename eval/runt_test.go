package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

type fakeT struct {
	mu     sync.Mutex
	name   string
	errors []string
	logs   []string
	subs   []*fakeT
}

func (f *fakeT) Helper() {}
func (f *fakeT) Logf(format string, a ...any) {
	f.logs = append(f.logs, fmt.Sprintf(format, a...))
}
func (f *fakeT) Errorf(format string, a ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, a...))
}
func (f *fakeT) Run(name string, fn func(T)) bool {
	sub := &fakeT{name: name}
	f.subs = append(f.subs, sub)
	fn(sub)
	return len(sub.errors) == 0
}

func TestRunWith(t *testing.T) {
	ft := &fakeT{}
	agent := func(_ context.Context, c Case) (Run, error) { return bashRun("The output is hi."), nil }
	RunWith(ft, evalSet(), agent, Criteria{TrajectoryThreshold: Threshold(1)})

	assert.Empty(t, ft.errors)
	if assert.Len(t, ft.subs, 2) {
		assert.Equal(t, "good", ft.subs[0].name)
		assert.Empty(t, ft.subs[0].errors)
		assert.Len(t, ft.subs[0].logs, 1, "passing case logs its report")
		assert.Equal(t, "bad-tools", ft.subs[1].name)
		if assert.Len(t, ft.subs[1].errors, 1) {
			msg := ft.subs[1].errors[0]
			assert.Contains(t, msg, "eval case failed")
			assert.Contains(t, msg, "expected tools (exact): Read()")
			assert.Contains(t, msg, "actual tools: Bash(")
		}
	}
}

func TestRunWithRepeatsAndErrors(t *testing.T) {
	ft := &fakeT{}
	calls := 0
	agent := func(context.Context, Case) (Run, error) { calls++; return bashRun("x"), nil }
	RunWith(ft, evalSet(), agent, Criteria{}, WithRepeats(3))
	assert.Equal(t, 6, calls)

	ft = &fakeT{}
	RunWith(ft, Set{Cases: []Case{{ID: "a"}}}, agent, Criteria{})
	if assert.Len(t, ft.errors, 1) {
		assert.True(t, strings.Contains(ft.errors[0], "invalid eval set"))
	}
	assert.Empty(t, ft.subs)
}

// RunT with a real *testing.T: passing cases only.
func TestRunTReal(t *testing.T) {
	set := Set{ID: "s", Cases: []Case{
		{ID: "one", Prompt: "p", ExpectedTools: []ToolCall{{Name: "Bash"}}, IgnoreArgs: true, Match: InOrder},
		{ID: "two", Prompt: "p", ResponseContains: []string{"hi"}},
	}}
	RunT(t, set, func(context.Context, Case) (Run, error) { return bashRun("hi"), nil }, Criteria{TrajectoryThreshold: Threshold(1)})
}
