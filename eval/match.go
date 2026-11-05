package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// MatchType selects how an actual tool-call trajectory is compared with the
// expected one. The semantics are the same as ADK's TrajectoryEvaluator.
type MatchType int

const (
	// Exact: same calls, same order, no extras.
	Exact MatchType = iota
	// InOrder: every expected call appears in the actual trajectory in the
	// expected order; extra calls may appear anywhere in between.
	InOrder
	// AnyOrder: every expected call appears in the actual trajectory in any
	// order; extra calls are allowed. Each actual call satisfies at most one
	// expected call.
	AnyOrder
)

var matchNames = [...]string{"exact", "in_order", "any_order"}

func (m MatchType) String() string {
	if m < 0 || int(m) >= len(matchNames) {
		return fmt.Sprintf("MatchType(%d)", int(m))
	}
	return matchNames[m]
}

// MarshalText implements encoding.TextMarshaler ("exact", "in_order", "any_order").
func (m MatchType) MarshalText() ([]byte, error) {
	if m < 0 || int(m) >= len(matchNames) {
		return nil, fmt.Errorf("eval: invalid match type %d", int(m))
	}
	return []byte(matchNames[m]), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. An empty value is Exact.
func (m *MatchType) UnmarshalText(b []byte) error {
	s := strings.ToLower(strings.TrimSpace(string(b)))
	if s == "" {
		*m = Exact
		return nil
	}
	for i, n := range matchNames {
		if s == n {
			*m = MatchType(i)
			return nil
		}
	}
	return fmt.Errorf("eval: unknown match type %q (want exact, in_order or any_order)", b)
}

// MatchOption tunes MatchTrajectory.
type MatchOption func(*matchOpts)

type matchOpts struct {
	subset bool
	mcp    bool
}

// ArgsSubset makes an expected call match when its args are a subset of the
// actual args: every key in an expected object must be present in the actual
// one with an equal (recursively subset) value. Real args vary (paths,
// timestamps), so goldens usually pin only the keys that matter.
func ArgsSubset() MatchOption { return func(o *matchOpts) { o.subset = true } }

// MCPNames lets an expected bare tool name ("search") match an actual
// MCP-prefixed name ("mcp__server__search"). It is opt-in because the same
// bare name can exist on several servers; without it names must be equal, so
// a golden may also spell out the full "mcp__server__tool" name.
func MCPNames() MatchOption { return func(o *matchOpts) { o.mcp = true } }

// MatchTrajectory reports whether actual satisfies expected under mt. With
// ignoreArgs only names are compared. Args are compared as JSON values (key
// order and whitespace are irrelevant; absent, null and {} are all "no
// args"). Result/IsError fields are never compared.
func MatchTrajectory(actual, expected []ToolCall, mt MatchType, ignoreArgs bool, opts ...MatchOption) bool {
	var o matchOpts
	for _, f := range opts {
		f(&o)
	}
	eq := func(a, e ToolCall) bool { return callMatches(a, e, ignoreArgs, o) }
	switch mt {
	case Exact:
		if len(actual) != len(expected) {
			return false
		}
		for i := range expected {
			if !eq(actual[i], expected[i]) {
				return false
			}
		}
		return true
	case InOrder:
		i := 0
		for _, a := range actual {
			if i < len(expected) && eq(a, expected[i]) {
				i++
			}
		}
		return i == len(expected)
	case AnyOrder:
		return anyOrder(actual, expected, eq)
	}
	return false
}

// anyOrder finds a matching that assigns every expected call a distinct
// actual call (Kuhn's augmenting paths), so a loose expected call cannot
// steal the only actual call a stricter one needs.
func anyOrder(actual, expected []ToolCall, eq func(a, e ToolCall) bool) bool {
	if len(expected) > len(actual) {
		return false
	}
	owner := make([]int, len(actual)) // actual index -> expected index, -1 free
	for i := range owner {
		owner[i] = -1
	}
	var try func(e int, seen []bool) bool
	try = func(e int, seen []bool) bool {
		for a := range actual {
			if seen[a] || !eq(actual[a], expected[e]) {
				continue
			}
			seen[a] = true
			if owner[a] < 0 || try(owner[a], seen) {
				owner[a] = e
				return true
			}
		}
		return false
	}
	for e := range expected {
		if !try(e, make([]bool, len(actual))) {
			return false
		}
	}
	return true
}

func callMatches(a, e ToolCall, ignoreArgs bool, o matchOpts) bool {
	if !nameMatches(a.Name, e.Name, o.mcp) {
		return false
	}
	if ignoreArgs {
		return true
	}
	return argsMatch(a.Args, e.Args, o.subset)
}

func nameMatches(actual, expected string, mcp bool) bool {
	if actual == expected {
		return true
	}
	if mcp && strings.HasPrefix(actual, "mcp__") {
		if parts := strings.SplitN(actual, "__", 3); len(parts) == 3 {
			return parts[2] == expected
		}
	}
	return false
}

func decodeArgs(raw json.RawMessage) (any, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, true
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil, true
	}
	return v, true
}

func argsMatch(actual, expected json.RawMessage, subset bool) bool {
	a, okA := decodeArgs(actual)
	e, okE := decodeArgs(expected)
	if !okA || !okE {
		return bytes.Equal(bytes.TrimSpace(actual), bytes.TrimSpace(expected))
	}
	if subset {
		return jsonSubset(a, e)
	}
	return reflect.DeepEqual(a, e)
}

// jsonSubset reports whether exp is contained in act. Objects are compared by
// key containment, arrays element-wise (same length), scalars by equality.
func jsonSubset(act, exp any) bool {
	switch e := exp.(type) {
	case nil:
		return true // nothing pinned
	case map[string]any:
		a, ok := act.(map[string]any)
		if !ok {
			return false
		}
		for k, ev := range e {
			av, present := a[k]
			if !present || !jsonSubset(av, ev) {
				return false
			}
		}
		return true
	case []any:
		a, ok := act.([]any)
		if !ok || len(a) != len(e) {
			return false
		}
		for i := range e {
			if !jsonSubset(a[i], e[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(act, exp)
	}
}
