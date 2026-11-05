package eval

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tc(name string, args ...string) ToolCall {
	c := ToolCall{Name: name}
	if len(args) > 0 {
		c.Args = json.RawMessage(args[0])
	}
	return c
}

func tcs(names ...string) []ToolCall {
	var out []ToolCall
	for _, n := range names {
		out = append(out, tc(n))
	}
	return out
}

func TestMatchTrajectory(t *testing.T) {
	adkActual := tcs("T1", "T1.1", "T2", "T2.1", "T2.2", "T3", "T3.1")
	tests := []struct {
		name     string
		actual   []ToolCall
		expected []ToolCall
		mt       MatchType
		want     bool
	}{
		// ADK documented examples.
		{"adk in-order passes with extras", adkActual, tcs("T1", "T2", "T3"), InOrder, true},
		{"adk exact fails with extras", adkActual, tcs("T1", "T2", "T3"), Exact, false},
		{"adk any-order passes", adkActual, tcs("T3", "T1", "T2"), AnyOrder, true},
		{"adk in-order fails wrong order", adkActual, tcs("T3", "T1", "T2"), InOrder, false},

		{"exact equal", tcs("a", "b"), tcs("a", "b"), Exact, true},
		{"exact wrong order", tcs("b", "a"), tcs("a", "b"), Exact, false},
		{"exact shorter actual", tcs("a"), tcs("a", "b"), Exact, false},
		{"exact both empty", nil, nil, Exact, true},
		{"exact expects none but got one", tcs("a"), nil, Exact, false},

		{"in-order empty expected", tcs("a"), nil, InOrder, true},
		{"in-order missing", tcs("a"), tcs("a", "b"), InOrder, false},
		{"in-order repeated", tcs("a", "x", "a"), tcs("a", "a"), InOrder, true},
		{"in-order repeated missing second", tcs("a", "x"), tcs("a", "a"), InOrder, false},

		{"any-order duplicates need two", tcs("a", "b"), tcs("a", "a"), AnyOrder, false},
		{"any-order duplicates ok", tcs("a", "b", "a"), tcs("a", "a"), AnyOrder, true},
		{"any-order more expected than actual", tcs("a"), tcs("a", "b"), AnyOrder, false},
		{"any-order empty", nil, nil, AnyOrder, true},

		{"invalid match type", tcs("a"), tcs("a"), MatchType(9), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchTrajectory(tt.actual, tt.expected, tt.mt, true))
		})
	}
}

func TestMatchArgs(t *testing.T) {
	a := []ToolCall{tc("Bash", `{"command":"ls","cwd":"/x","opts":{"a":1,"b":[1,2]}}`)}
	tests := []struct {
		name     string
		expected string
		ignore   bool
		opts     []MatchOption
		want     bool
	}{
		{"key order and spacing", `{ "opts": {"b":[1,2],"a":1}, "cwd":"/x", "command":"ls" }`, false, nil, true},
		{"different value", `{"command":"pwd","cwd":"/x","opts":{"a":1,"b":[1,2]}}`, false, nil, false},
		{"missing key without subset", `{"command":"ls"}`, false, nil, false},
		{"missing key ignoring args", `{"command":"nope"}`, true, nil, true},
		{"subset top-level", `{"command":"ls"}`, false, []MatchOption{ArgsSubset()}, true},
		{"subset nested", `{"opts":{"a":1}}`, false, []MatchOption{ArgsSubset()}, true},
		{"subset wrong value", `{"opts":{"a":2}}`, false, []MatchOption{ArgsSubset()}, false},
		{"subset array must match", `{"opts":{"b":[1]}}`, false, []MatchOption{ArgsSubset()}, false},
		{"subset array equal", `{"opts":{"b":[1,2]}}`, false, []MatchOption{ArgsSubset()}, true},
		{"subset key absent", `{"zzz":1}`, false, []MatchOption{ArgsSubset()}, false},
		{"subset object vs scalar", `{"command":{"x":1}}`, false, []MatchOption{ArgsSubset()}, false},
		{"subset array vs scalar", `{"command":[1]}`, false, []MatchOption{ArgsSubset()}, false},
		{"subset null pins nothing", `{"command":null}`, false, []MatchOption{ArgsSubset()}, true},
		{"no args expected, subset", ``, false, []MatchOption{ArgsSubset()}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := []ToolCall{tc("Bash", tt.expected)}
			if tt.expected == "" {
				exp = []ToolCall{tc("Bash")}
			}
			assert.Equal(t, tt.want, MatchTrajectory(a, exp, Exact, tt.ignore, tt.opts...))
		})
	}
}

func TestMatchEmptyAndInvalidArgs(t *testing.T) {
	for _, empty := range []string{``, `null`, `{}`, ` `} {
		assert.True(t, MatchTrajectory([]ToolCall{tc("A")}, []ToolCall{tc("A", empty)}, Exact, false), empty)
		assert.True(t, MatchTrajectory([]ToolCall{tc("A", empty)}, []ToolCall{tc("A")}, Exact, false), empty)
	}
	assert.False(t, MatchTrajectory([]ToolCall{tc("A", `{"k":1}`)}, []ToolCall{tc("A")}, Exact, false))
	// Unparseable args fall back to byte equality.
	assert.True(t, MatchTrajectory([]ToolCall{tc("A", `{bad`)}, []ToolCall{tc("A", `{bad`)}, Exact, false))
	assert.False(t, MatchTrajectory([]ToolCall{tc("A", `{bad`)}, []ToolCall{tc("A", `{"k":1}`)}, Exact, false))
}

func TestMatchMCPNames(t *testing.T) {
	actual := tcs("mcp__github__search")
	assert.False(t, MatchTrajectory(actual, tcs("search"), Exact, true))
	assert.True(t, MatchTrajectory(actual, tcs("search"), Exact, true, MCPNames()))
	assert.True(t, MatchTrajectory(actual, tcs("mcp__github__search"), Exact, true))
	assert.False(t, MatchTrajectory(actual, tcs("fetch"), Exact, true, MCPNames()))
	assert.False(t, MatchTrajectory(tcs("mcp__x"), tcs("x"), Exact, true, MCPNames()))
	assert.False(t, MatchTrajectory(tcs("Bash"), tcs("sh"), Exact, true, MCPNames()))
}

func TestAnyOrderNeedsAugmentingPath(t *testing.T) {
	// The loose expected call A matches both actual calls, the strict one only
	// the first; a greedy matcher would give the first to A and fail.
	actual := []ToolCall{tc("T", `{"k":1}`), tc("T", `{"k":2}`)}
	expected := []ToolCall{tc("T"), tc("T", `{"k":1}`)}
	assert.True(t, MatchTrajectory(actual, expected, AnyOrder, false, ArgsSubset()))
}

func TestMatchTypeText(t *testing.T) {
	for i, n := range []string{"exact", "in_order", "any_order"} {
		mt := MatchType(i)
		assert.Equal(t, n, mt.String())
		b, err := json.Marshal(mt)
		require.NoError(t, err)
		assert.Equal(t, `"`+n+`"`, string(b))
		var got MatchType
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, mt, got)
	}
	var m MatchType
	require.NoError(t, json.Unmarshal([]byte(`"IN_ORDER"`), &m))
	assert.Equal(t, InOrder, m)
	require.NoError(t, m.UnmarshalText(nil))
	assert.Equal(t, Exact, m)
	assert.Error(t, json.Unmarshal([]byte(`"fuzzy"`), &m))
	_, err := MatchType(7).MarshalText()
	assert.Error(t, err)
	assert.Equal(t, "MatchType(7)", MatchType(7).String())
}
