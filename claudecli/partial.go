package claudecli

import "encoding/json"

// Partial (token-level) streaming is enabled by the CLI flag
// --include-partial-messages. It emits lines of type "stream_event" whose
// "event" field is a raw Anthropic streaming event (message_start,
// content_block_start, content_block_delta, content_block_stop, message_delta,
// message_stop). These types model the subset needed to assemble live text and
// thinking deltas.

// Anthropic streaming event type constants.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
)

// Delta block-type constants (the inner delta.type of a content_block_delta).
const (
	DeltaText      = "text_delta"
	DeltaThinking  = "thinking_delta"
	DeltaSignature = "signature_delta"
	DeltaInputJSON = "input_json_delta"
)

// AnthropicEvent is one streaming event from a partial-messages stream.
type AnthropicEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index,omitempty"`
	Delta        *AnthropicDelta `json:"delta,omitempty"`
	ContentBlock *struct {
		Type string `json:"type"`
	} `json:"content_block,omitempty"`
}

// AnthropicDelta is the incremental payload of a content_block_delta event.
type AnthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`         // text_delta
	Thinking    string `json:"thinking,omitempty"`     // thinking_delta
	PartialJSON string `json:"partial_json,omitempty"` // input_json_delta
}

// IsPartial reports whether this event line is a partial stream_event wrapper.
func (e *StreamEvent) IsPartial() bool { return e.Type == "stream_event" }

// Partial decodes the wrapped Anthropic event, or nil if this is not a
// stream_event line or the payload is absent/invalid.
func (e *StreamEvent) Partial() *AnthropicEvent {
	if !e.IsPartial() || len(e.Event) == 0 {
		return nil
	}
	var ev AnthropicEvent
	if err := json.Unmarshal(e.Event, &ev); err != nil {
		return nil
	}
	return &ev
}

// TextDelta returns the incremental assistant text for a partial event, or ""
// if this event carries no text delta.
func (e *StreamEvent) TextDelta() string {
	p := e.Partial()
	if p == nil || p.Type != EventContentBlockDelta || p.Delta == nil || p.Delta.Type != DeltaText {
		return ""
	}
	return p.Delta.Text
}

// ThinkingDelta returns the incremental thinking text for a partial event, or
// "" if this event carries no thinking delta.
func (e *StreamEvent) ThinkingDelta() string {
	p := e.Partial()
	if p == nil || p.Type != EventContentBlockDelta || p.Delta == nil || p.Delta.Type != DeltaThinking {
		return ""
	}
	return p.Delta.Thinking
}
