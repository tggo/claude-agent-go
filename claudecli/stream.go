// Package claudecli provides utilities for interacting with Claude CLI output.
package claudecli

import "encoding/json"

// StreamEvent represents a single event from Claude CLI's stream-json output.
// Each line of output is a JSON object with a "type" field.
type StreamEvent struct {
	// Type is the event type (e.g., "assistant", "result", "system", "error").
	Type string `json:"type"`

	// Subtype further classifies the event (e.g., "init", "success").
	Subtype string `json:"subtype,omitempty"`

	// CostUSD is the cost key used by older CLI builds. Newer builds (2.x)
	// emit total_cost_usd instead — use Cost() to read whichever is present.
	CostUSD float64 `json:"cost_usd,omitempty"`

	// TotalCostUSD is the cost key emitted by current CLI builds.
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`

	// DurationMS is the total duration in milliseconds (result events).
	DurationMS int64 `json:"duration_ms,omitempty"`

	// DurationAPIMS is the API-specific duration in milliseconds.
	DurationAPIMS int64 `json:"duration_api_ms,omitempty"`

	// IsError indicates if this is an error result.
	IsError bool `json:"is_error,omitempty"`

	// NumTurns is the number of agentic turns used (result events).
	NumTurns int `json:"num_turns,omitempty"`

	// SessionID is the Claude session ID (init and result events).
	SessionID string `json:"session_id,omitempty"`

	// Result contains the final result text (result events).
	Result string `json:"result,omitempty"`

	// Message is the raw assistant message object (assistant events). It is a
	// structured object on current CLI builds, so it is kept raw and decoded
	// on demand by AssistantText/AssistantMessage rather than typed as string.
	Message json.RawMessage `json:"message,omitempty"`

	// Event is the raw wrapped Anthropic streaming event (stream_event lines,
	// emitted under --include-partial-messages). Decode via Partial().
	Event json.RawMessage `json:"event,omitempty"`

	// Raw is the original line bytes, set by ParseStreamLine. Useful for event
	// types the SDK does not model (e.g. rate_limit_event) — Unmarshal it
	// yourself for the fields you need.
	Raw []byte `json:"-"`
}

// ParseStreamLine parses a single line from stream-json output.
// Returns nil if the line is empty or not valid JSON.
func ParseStreamLine(line []byte) *StreamEvent {
	if len(line) == 0 {
		return nil
	}
	var event StreamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return nil
	}
	event.Raw = line
	return &event
}

// IsRateLimit reports whether this is a rate_limit_event line. The CLI emits
// these periodically; surface them (via RunStream's ProgressFunc) to back off.
// Decode StreamEvent.Raw for the specific fields.
func (e *StreamEvent) IsRateLimit() bool {
	return e.Type == "rate_limit_event"
}

// IsResult returns true if this event is the final result.
func (e *StreamEvent) IsResult() bool {
	return e.Type == "result"
}

// IsAssistant returns true if this is an assistant message event.
func (e *StreamEvent) IsAssistant() bool {
	return e.Type == msgTypeAssistant
}

// Cost returns the invocation cost, preferring the current total_cost_usd key
// and falling back to the legacy cost_usd key.
func (e *StreamEvent) Cost() float64 {
	if e.TotalCostUSD != 0 {
		return e.TotalCostUSD
	}
	return e.CostUSD
}

// AssistantMessage decodes the raw assistant message object, or nil if this is
// not an assistant event or the payload is absent/invalid.
func (e *StreamEvent) AssistantMessage() *AssistantMessage {
	if !e.IsAssistant() || len(e.Message) == 0 {
		return nil
	}
	var m AssistantMessage
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return nil
	}
	return &m
}

// AssistantText concatenates the text blocks of an assistant event, or "" if
// there are none.
func (e *StreamEvent) AssistantText() string {
	m := e.AssistantMessage()
	if m == nil {
		return ""
	}
	var s string
	for _, b := range m.Content {
		if b.Type == BlockText && b.Text != "" {
			if s != "" {
				s += "\n"
			}
			s += b.Text
		}
	}
	return s
}

// ExtractTextFromStream collects all text from assistant messages in a stream
// output. Also returns the final result event if found. The result text is
// authoritative when present; assistant text is the fallback.
func ExtractTextFromStream(lines [][]byte) (text string, result *StreamEvent) {
	var textParts []string
	for _, line := range lines {
		event := ParseStreamLine(line)
		if event == nil {
			continue
		}
		if t := event.AssistantText(); t != "" {
			textParts = append(textParts, t)
		}
		if event.IsResult() {
			result = event
		}
	}
	if result != nil && result.Result != "" {
		return result.Result, result
	}
	joined := ""
	for i, p := range textParts {
		if i > 0 {
			joined += "\n"
		}
		joined += p
	}
	return joined, result
}
