package eval

import (
	"context"
	"testing"
)

// T is the part of *testing.T that RunWith needs, so eval runs can be tested
// with a fake. Use RunT for the real thing.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	// Run runs f as a named subtest and reports whether it passed.
	Run(name string, f func(T)) bool
}

type stdT struct{ *testing.T }

func (s stdT) Run(name string, f func(T)) bool {
	return s.T.Run(name, func(t *testing.T) { f(stdT{t}) })
}

// RunT evaluates every case of set as a subtest of t. A failing case reports
// its detailed report (expected vs actual tools, scores, metrics) via Errorf.
// Cases run sequentially; only WithRepeats applies.
func RunT(t *testing.T, set Set, agent AgentFunc, c Criteria, opts ...EvalOption) {
	t.Helper()
	RunWith(stdT{t}, set, agent, c, opts...)
}

// RunWith is RunT over the minimal T interface.
func RunWith(t T, set Set, agent AgentFunc, c Criteria, opts ...EvalOption) {
	t.Helper()
	if err := set.Validate(); err != nil {
		t.Errorf("invalid eval set: %v", err)
		return
	}
	cfg := newConfig(opts)
	for _, cs := range set.Cases {
		t.Run(cs.ID, func(t T) {
			t.Helper()
			res := evalCase(context.Background(), cs, agent, c, cfg.repeats, cfg.minPassRate)
			if res.Pass {
				t.Logf("%s", res.Detail())
				return
			}
			t.Errorf("eval case failed:\n%s", res.Detail())
		})
	}
}
