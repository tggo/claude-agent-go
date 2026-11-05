package claudecli

import (
	"encoding/json"
	"fmt"
)

// Violation is one broken stream invariant found by CheckInvariants.
type Violation struct {
	// Index is the position in the events slice of the offending event.
	Index int
	// Rule is a stable machine-readable name of the broken rule.
	Rule string
	// Detail is a human-readable explanation.
	Detail string
}

func (v Violation) String() string { return fmt.Sprintf("event %d: %s: %s", v.Index, v.Rule, v.Detail) }

// Names of the rules CheckInvariants reports.
const (
	RuleUnmatchedToolUse    = "tool_use_without_result"
	RuleOrphanToolResult    = "tool_result_without_use"
	RuleDuplicateToolResult = "duplicate_tool_result"
	RuleDuplicateToolUse    = "duplicate_tool_use"
	RuleMultipleResults     = "multiple_result_events"
	RuleEventAfterResult    = "event_after_result"
	RuleSessionMismatch     = "session_id_mismatch"
	RuleUnknownParent       = "unknown_parent_tool_use"
)

// CheckInvariants verifies structural rules every well-formed stream-json run
// obeys, so tests can assert on a whole stream instead of hand-picked fields:
//
//   - every tool_use id gets exactly one tool_result, and no result lacks a use
//   - at most one result event, and nothing follows it
//   - session_id is the same on every event that carries one
//   - a sub-agent event's parent_tool_use_id names a tool_use seen earlier
//
// A stream that ends without a result event (cancelled or truncated) is not a
// violation by itself, but its dangling tool_use ids are reported. Violations
// come back in event order; a clean stream yields nil.
func CheckInvariants(events []StreamEvent) []Violation {
	var out []Violation
	add := func(i int, rule, f string, a ...any) {
		out = append(out, Violation{Index: i, Rule: rule, Detail: fmt.Sprintf(f, a...)})
	}
	uses := map[string]int{}    // tool_use id -> index of the event
	results := map[string]int{} // tool_use id -> index of its result
	var order []string
	session, sessionAt := "", -1
	resultAt := -1

	for i := range events {
		ev := &events[i]
		if resultAt >= 0 {
			if ev.IsResult() {
				add(i, RuleMultipleResults, "second result event (first at %d)", resultAt)
			} else {
				add(i, RuleEventAfterResult, "%q event after result at %d", ev.Type, resultAt)
			}
		} else if ev.IsResult() {
			resultAt = i
		}
		if ev.SessionID != "" {
			if session == "" {
				session, sessionAt = ev.SessionID, i
			} else if ev.SessionID != session {
				add(i, RuleSessionMismatch, "session_id %q, want %q (first seen at %d)", ev.SessionID, session, sessionAt)
			}
		}
		if ev.ParentToolUseID != "" {
			if _, ok := uses[ev.ParentToolUseID]; !ok {
				add(i, RuleUnknownParent, "parent_tool_use_id %q matches no earlier tool_use", ev.ParentToolUseID)
			}
		}
		if ev.Type != msgTypeAssistant && ev.Type != "user" {
			continue
		}
		for _, b := range eventBlocks(ev) {
			switch b.Type {
			case BlockToolUse:
				if _, dup := uses[b.ID]; dup {
					add(i, RuleDuplicateToolUse, "tool_use id %q seen twice", b.ID)
					continue
				}
				uses[b.ID] = i
				order = append(order, b.ID)
			case BlockToolResult:
				if _, ok := uses[b.ToolUseID]; !ok {
					add(i, RuleOrphanToolResult, "tool_result for unknown tool_use %q", b.ToolUseID)
				} else if first, dup := results[b.ToolUseID]; dup {
					add(i, RuleDuplicateToolResult, "second tool_result for %q (first at %d)", b.ToolUseID, first)
				} else {
					results[b.ToolUseID] = i
				}
			}
		}
	}
	for _, id := range order {
		if _, ok := results[id]; !ok {
			add(uses[id], RuleUnmatchedToolUse, "tool_use %q has no tool_result", id)
		}
	}
	sortViolations(out)
	return out
}

// eventBlocks decodes the content blocks of an assistant or user event. User
// events carry content as an array of blocks (tool results) or as a plain
// string (a prompt); the latter yields no blocks.
func eventBlocks(ev *StreamEvent) []ContentBlock {
	if len(ev.Message) == 0 {
		return nil
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil {
		return nil
	}
	var blocks []ContentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil
	}
	return blocks
}

func sortViolations(v []Violation) {
	// insertion sort keeps equal-index violations in discovery order (stable).
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j].Index < v[j-1].Index; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
