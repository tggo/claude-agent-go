// Package signal detects out-of-band outcome markers an agent may embed in its
// output text (e.g. "<<<TASK_COMPLETED>>>"). It is marker-agnostic: callers
// define their own markers and optional natural-language fallback phrases.
package signal

import "strings"

// Set maps caller-defined outcome names to the literal markers that indicate
// them, checked in the order given by Priority.
type Set struct {
	// Markers maps an outcome name to the literal string to search for.
	Markers map[string]string
	// Priority lists outcome names most-specific first; the first match wins.
	// Names absent from Priority are checked after, in unspecified order.
	Priority []string
}

// Detect returns the name of the first matching marker, or "" if none match.
func (s Set) Detect(text string) string {
	for _, name := range s.Priority {
		if marker, ok := s.Markers[name]; ok && marker != "" && strings.Contains(text, marker) {
			return name
		}
	}
	for name, marker := range s.Markers {
		if contains(s.Priority, name) {
			continue
		}
		if marker != "" && strings.Contains(text, marker) {
			return name
		}
	}
	return ""
}

// ContainsAny reports whether text contains any of the given phrases
// (case-insensitive). Useful as a heuristic fallback when an explicit marker is
// absent — e.g. detecting "already implemented" / "nothing to commit".
func ContainsAny(text string, phrases []string) bool {
	lower := strings.ToLower(text)
	for _, p := range phrases {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
