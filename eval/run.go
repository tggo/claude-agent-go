// Package eval evaluates agents built on this SDK, in the spirit of Google
// ADK's `adk eval`: observe what an agent did (tool calls, final response,
// token/cost/latency metrics), compare it with a golden case, and score it
// with deterministic metrics (trajectory match, ROUGE) and an LLM judge that
// runs on the Claude CLI itself.
//
// The building blocks are independent:
//
//   - Run / FromStream / FromRecording / Collector: the observed run model.
//   - MatchTrajectory: tool-call trajectory comparison.
//   - RougeL / Rouge1 / Contains / Matches: response scoring.
//   - Judge / ClaudeJudge: rubric scoring by a model.
//   - Set / Case / LoadSet / CaseFromRecording: golden eval sets on disk.
//   - Evaluate / RunT: score a set against an agent and report.
package eval

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
)

// ToolCall is one tool invocation made by the agent (or expected of it).
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
	// Result is the tool's output text, when a matching tool_result was seen.
	// It is observed data only and never compared by MatchTrajectory.
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// ParentToolUseID is set when a sub-agent made the call: the id of the
	// tool_use that spawned it. Sub-agent calls count in Metrics.ToolCalls
	// like any other; filter on this to separate them.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`
}

// Metrics summarises the cost and latency of a run.
type Metrics struct {
	// ToolCalls is the number of tool invocations.
	ToolCalls int
	// Inferences is the number of distinct assistant messages (model calls).
	Inferences int
	// Turns is the CLI-reported agentic turn count (falls back to Inferences).
	Turns int

	InputTokens     int
	OutputTokens    int
	CacheReadTokens int

	CostUSD float64

	// Duration is the wall time of the run. ModelTime and ToolTime split it;
	// they are only populated when timing offsets are known (recordings and
	// live Collectors). ToolTime is the union of the intervals between each
	// tool_use and its tool_result, so parallel tools are not double-counted.
	Duration  time.Duration
	ModelTime time.Duration
	ToolTime  time.Duration
}

// Run is everything observed about one agent run.
type Run struct {
	ToolCalls []ToolCall
	Response  string
	Metrics   Metrics
	SessionID string
}

// FromStream builds a Run from raw stream-json lines. No timing is available,
// so Metrics.Duration comes from the result event and Model/ToolTime stay 0.
func FromStream(lines [][]byte) Run {
	b := newBuilder(false)
	for _, l := range lines {
		if ev := claudecli.ParseStreamLine(l); ev != nil {
			b.add(ev, 0)
		}
	}
	return b.finish()
}

// FromRecording builds a Run from a recording, using its _cag_t offsets for
// the model/tool time split.
func FromRecording(rec *recording.Recording) Run {
	timed := false
	for _, l := range rec.Lines {
		if l.Offset > 0 {
			timed = true
			break
		}
	}
	b := newBuilder(timed)
	for _, l := range rec.Lines {
		if ev := l.Event(); ev != nil {
			b.add(ev, l.Offset)
		}
	}
	return b.finish()
}

// FromRecordingFile reads a recording file and builds its Run.
func FromRecordingFile(path string) (Run, error) {
	rec, err := recording.ReadFile(path)
	if err != nil {
		return Run{}, err
	}
	return FromRecording(rec), nil
}

// Collector builds a Run live from a runner stream. Pass Progress as the
// runner.ProgressFunc of RunStream, then read Run. Offsets are measured with
// time.Now from the first event (or from NewCollector). Safe for concurrent
// use.
type Collector struct {
	mu    sync.Mutex
	b     *builder
	start time.Time
	now   func() time.Time
}

// NewCollector returns a Collector whose clock starts now.
func NewCollector() *Collector {
	return &Collector{b: newBuilder(true), start: time.Now(), now: time.Now}
}

// Progress consumes one stream event. Its signature matches runner.ProgressFunc.
func (c *Collector) Progress(ev claudecli.StreamEvent, _ int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.b == nil {
		c.b = newBuilder(true)
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.start.IsZero() {
		c.start = c.now()
	}
	c.b.add(&ev, c.now().Sub(c.start))
}

// Run returns the Run observed so far. It may be called repeatedly.
func (c *Collector) Run() Run {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.b == nil {
		return Run{}
	}
	return c.b.snapshot().finish()
}

type interval struct{ from, to time.Duration }

type builder struct {
	timed bool

	calls  []ToolCall
	callIx map[string]int
	useAt  map[string]time.Duration
	spans  []interval

	msgs     map[string]struct{}
	usage    map[string]claudecli.MessageUsage
	anonInfs int
	anonUse  claudecli.MessageUsage

	lastText string
	last     time.Duration

	sessionID  string
	hasResult  bool
	resultText string
	numTurns   int
	durationMS int64
	cost       float64
	// resultUsage is the result event's usage: the CLI's authoritative run
	// totals. Per-message usage in stream-json is a mid-stream snapshot (output
	// tokens are mostly missing), so it is only the fallback.
	resultUsage *claudecli.MessageUsage
}

func newBuilder(timed bool) *builder {
	return &builder{
		timed:  timed,
		callIx: map[string]int{},
		useAt:  map[string]time.Duration{},
		msgs:   map[string]struct{}{},
		usage:  map[string]claudecli.MessageUsage{},
	}
}

// snapshot returns an independent copy so finish can run mid-stream.
func (b *builder) snapshot() *builder {
	cp := *b
	cp.calls = append([]ToolCall(nil), b.calls...)
	cp.spans = append([]interval(nil), b.spans...)
	return &cp
}

type userMessage struct {
	Content []claudecli.ContentBlock `json:"content"`
}

func (b *builder) add(ev *claudecli.StreamEvent, off time.Duration) {
	if off > b.last {
		b.last = off
	}
	if ev.SessionID != "" {
		b.sessionID = ev.SessionID
	}
	switch ev.Type {
	case "assistant":
		am := ev.AssistantMessage()
		if am == nil {
			return
		}
		b.addAssistant(am, off, ev.ParentToolUseID)
	case "user":
		var um userMessage
		if json.Unmarshal(ev.Message, &um) != nil {
			return // plain-string content: a prompt echo, not a tool result
		}
		for _, blk := range um.Content {
			if blk.Type == "tool_result" {
				b.addResult(blk, off)
			}
		}
	case "result":
		b.hasResult = true
		b.resultText = ev.Result
		b.numTurns = ev.NumTurns
		b.durationMS = ev.DurationMS
		b.cost = ev.Cost()
		var ru struct {
			Usage *claudecli.MessageUsage `json:"usage"`
		}
		if json.Unmarshal(ev.Raw, &ru) == nil && ru.Usage != nil {
			b.resultUsage = ru.Usage
		}
	}
}

func (b *builder) addAssistant(am *claudecli.AssistantMessage, off time.Duration, parent string) {
	if am.ID == "" {
		b.anonInfs++
		if am.Usage != nil {
			b.anonUse.InputTokens += am.Usage.InputTokens
			b.anonUse.OutputTokens += am.Usage.OutputTokens
			b.anonUse.CacheReadInputTokens += am.Usage.CacheReadInputTokens
		}
	} else {
		b.msgs[am.ID] = struct{}{}
		if am.Usage != nil {
			b.usage[am.ID] = *am.Usage // same message may repeat per block; keep the latest
		}
	}
	var text []string
	for _, blk := range am.Content {
		switch blk.Type {
		case "text":
			text = append(text, blk.Text)
		case "tool_use":
			if _, dup := b.callIx[blk.ID]; dup && blk.ID != "" {
				continue
			}
			if blk.ID != "" {
				b.callIx[blk.ID] = len(b.calls)
				b.useAt[blk.ID] = off
			}
			b.calls = append(b.calls, ToolCall{Name: blk.Name, Args: append(json.RawMessage(nil), blk.Input...), ParentToolUseID: parent})
		}
	}
	if len(text) > 0 && parent == "" { // a sub-agent's text is not the answer
		b.lastText = strings.Join(text, "\n")
	}
}

func (b *builder) addResult(blk claudecli.ContentBlock, off time.Duration) {
	i, ok := b.callIx[blk.ToolUseID]
	if !ok {
		return
	}
	b.calls[i].Result = flattenContent(blk.Content)
	b.calls[i].IsError = blk.IsError
	if at, ok := b.useAt[blk.ToolUseID]; ok && off >= at {
		b.spans = append(b.spans, interval{at, off})
	}
}

// flattenContent renders a tool_result content value (a string or an array of
// text blocks) as plain text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudecli.ContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, blk := range blocks {
			if blk.Type == "text" {
				parts = append(parts, blk.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

func (b *builder) finish() Run {
	m := Metrics{
		ToolCalls:  len(b.calls),
		Inferences: len(b.msgs) + b.anonInfs,
		CostUSD:    b.cost,
	}
	m.InputTokens, m.OutputTokens, m.CacheReadTokens = b.anonUse.InputTokens, b.anonUse.OutputTokens, b.anonUse.CacheReadInputTokens
	for _, u := range b.usage {
		m.InputTokens += u.InputTokens
		m.OutputTokens += u.OutputTokens
		m.CacheReadTokens += u.CacheReadInputTokens
	}
	if u := b.resultUsage; u != nil {
		m.InputTokens, m.OutputTokens, m.CacheReadTokens = u.InputTokens, u.OutputTokens, u.CacheReadInputTokens
	}
	m.Turns = b.numTurns
	if m.Turns == 0 {
		m.Turns = m.Inferences
	}
	if b.timed && b.last > 0 {
		m.Duration = b.last
		m.ToolTime = unionLen(b.spans)
		m.ModelTime = max(m.Duration-m.ToolTime, 0)
	} else {
		m.Duration = time.Duration(b.durationMS) * time.Millisecond
	}

	resp := b.lastText
	if b.hasResult && b.resultText != "" {
		resp = b.resultText
	}
	return Run{
		ToolCalls: append([]ToolCall(nil), b.calls...),
		Response:  resp,
		Metrics:   m,
		SessionID: b.sessionID,
	}
}

func unionLen(spans []interval) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	s := append([]interval(nil), spans...)
	sort.Slice(s, func(i, j int) bool { return s[i].from < s[j].from })
	var total time.Duration
	cur := s[0]
	for _, iv := range s[1:] {
		if iv.from <= cur.to {
			cur.to = max(cur.to, iv.to)
			continue
		}
		total += cur.to - cur.from
		cur = iv
	}
	return total + cur.to - cur.from
}
