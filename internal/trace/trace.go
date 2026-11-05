// Package trace turns recordings (see package recording) into a self-contained
// HTML page for inspecting what an agent run did: header metrics, an ordered
// timeline of text/thinking/tool calls/results, and a per-tool waterfall.
//
// Recording content is untrusted model and tool output; everything reaches the
// page through html/template and is escaped.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "embed"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
)

// previewLimit is the number of runes of tool output shown before the
// "expand" fold.
const previewLimit = 600

// Step kinds.
const (
	KindText       = "text"
	KindThinking   = "thinking"
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindSystem     = "system"
	KindRateLimit  = "rate_limit"
	KindResult     = "result"
)

// Step is one row of the timeline.
type Step struct {
	Kind     string
	Offset   time.Duration
	Name     string        // tool name (tool_use)
	Text     string        // text/thinking/result body, tool_use input, tool_result preview
	Full     string        // full tool_result text when Text is a truncated preview
	IsError  bool          // error tool_result or error result
	Duration time.Duration // tool_use: time until its tool_result; -1 if unknown
	Left     float64       // waterfall bar position, percent of wall time
	Width    float64       // waterfall bar width, percent of wall time
}

// HasDuration reports whether the step has a measured duration.
func (s Step) HasDuration() bool { return s.Duration >= 0 && s.Kind == KindToolUse }

// Model is the view model of one recording.
type Model struct {
	ID        string // html anchor
	Source    string // file name
	Label     string
	Attempt   int // 1-based try of a retried run; 0 for a single run
	Model     string
	Prompt    string
	WorkDir   string
	SessionID string
	StartedAt time.Time

	Status  string // ok | error | incomplete
	CostUSD float64
	Turns   int
	Wall    time.Duration

	InputTokens, OutputTokens, CacheRead, CacheWrite int

	Events, ToolCalls, Inferences, Errors int

	Steps []Step
}

// Title is the label, falling back to the source name, with the attempt
// number appended for retried runs.
func (m Model) Title() string {
	if m.Attempt > 0 {
		return m.baseTitle() + fmt.Sprintf(" · attempt %d", m.Attempt)
	}
	return m.baseTitle()
}

func (m Model) baseTitle() string {
	if m.Label != "" {
		return m.Label
	}
	if m.Source != "" {
		return m.Source
	}
	return "run"
}

// Build converts a recording into a Model.
func Build(rec *recording.Recording) Model {
	m := Model{
		Label: rec.Header.Label, Attempt: rec.Header.Attempt, Model: rec.Header.Model, Prompt: rec.Header.Prompt,
		WorkDir: rec.Header.WorkDir, StartedAt: rec.Header.StartedAt,
		Status: "incomplete", Events: len(rec.Lines),
	}
	var last time.Duration
	var sumIn, sumOut, sumCR, sumCW int
	seenMsg := map[string]bool{}
	open := map[string]int{} // tool_use id -> step index
	var resultUsage *claudecli.TokenUsage
	var resultDur time.Duration

	for _, ln := range rec.Lines {
		if ln.Offset > last {
			last = ln.Offset
		}
		ev := ln.Event()
		if ev == nil {
			continue
		}
		switch {
		case ev.Type == "system" && ev.Subtype == "init":
			var init claudecli.ClaudeOutputMessage
			_ = json.Unmarshal(ev.Raw, &init)
			if init.Model != "" && m.Model == "" {
				m.Model = init.Model
			}
			if m.SessionID == "" {
				m.SessionID = init.SessionID
			}
			m.Steps = append(m.Steps, Step{Kind: KindSystem, Offset: ln.Offset,
				Text: fmt.Sprintf("init: model %s, %d tools", init.Model, len(init.Tools))})
		case ev.IsRateLimit():
			m.Steps = append(m.Steps, Step{Kind: KindRateLimit, Offset: ln.Offset, Text: "rate limit event"})
		case ev.Type == "system":
			m.Steps = append(m.Steps, Step{Kind: KindSystem, Offset: ln.Offset, Text: "system: " + ev.Subtype})
		case ev.IsAssistant():
			msg := ev.AssistantMessage()
			if msg == nil {
				continue
			}
			if msg.ID == "" || !seenMsg[msg.ID] {
				m.Inferences++
				if msg.ID != "" {
					seenMsg[msg.ID] = true
				}
				if msg.Usage != nil {
					sumIn += msg.Usage.InputTokens
					sumOut += msg.Usage.OutputTokens
					sumCR += msg.Usage.CacheReadInputTokens
					sumCW += msg.Usage.CacheCreationInputTokens
				}
			}
			for _, b := range msg.Content {
				switch b.Type {
				case "text":
					m.Steps = append(m.Steps, Step{Kind: KindText, Offset: ln.Offset, Text: b.Text})
				case "thinking":
					m.Steps = append(m.Steps, Step{Kind: KindThinking, Offset: ln.Offset, Text: b.Thinking})
				case "tool_use":
					m.ToolCalls++
					open[b.ID] = len(m.Steps)
					m.Steps = append(m.Steps, Step{Kind: KindToolUse, Offset: ln.Offset,
						Name: b.Name, Text: prettyJSON(b.Input), Duration: -1})
				}
			}
		case ev.Type == "user":
			for _, b := range userBlocks(ev.Message) {
				if b.Type != "tool_result" {
					continue
				}
				text := flattenContent(b.Content)
				st := Step{Kind: KindToolResult, Offset: ln.Offset, IsError: b.IsError}
				if r := []rune(text); len(r) > previewLimit {
					st.Text = string(r[:previewLimit]) + "…"
					st.Full = text
				} else {
					st.Text = text
				}
				if i, ok := open[b.ToolUseID]; ok {
					m.Steps[i].Duration = ln.Offset - m.Steps[i].Offset
					st.Name = m.Steps[i].Name
					delete(open, b.ToolUseID)
				}
				if b.IsError {
					m.Errors++
				}
				m.Steps = append(m.Steps, st)
			}
		case ev.IsResult():
			var res claudecli.ClaudeOutputMessage
			_ = json.Unmarshal(ev.Raw, &res)
			m.Status = "ok"
			if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
				m.Status = "error"
				m.Errors++
			}
			m.CostUSD = ev.Cost()
			m.Turns = ev.NumTurns
			if ev.SessionID != "" {
				m.SessionID = ev.SessionID
			}
			resultUsage = res.Usage
			resultDur = time.Duration(ev.DurationMS) * time.Millisecond
			text := ev.Result
			if text == "" && len(res.StructuredOutput) > 0 {
				text = prettyJSON(res.StructuredOutput)
			}
			m.Steps = append(m.Steps, Step{Kind: KindResult, Offset: ln.Offset, Text: text,
				IsError: m.Status == "error", Name: ev.Subtype})
		}
	}

	if resultUsage != nil {
		m.InputTokens, m.OutputTokens = resultUsage.InputTokens, resultUsage.OutputTokens
		m.CacheRead, m.CacheWrite = resultUsage.CacheReadInputTokens, resultUsage.CacheCreationInputTokens
	} else {
		m.InputTokens, m.OutputTokens, m.CacheRead, m.CacheWrite = sumIn, sumOut, sumCR, sumCW
	}
	m.Wall = last
	if m.Wall == 0 {
		m.Wall = resultDur
	}
	m.Turns = max(m.Turns, 0)

	total := float64(m.Wall)
	for i := range m.Steps {
		s := &m.Steps[i]
		if s.Kind != KindToolUse || total <= 0 {
			continue
		}
		s.Left = pct(float64(s.Offset) / total)
		d := s.Duration
		if d < 0 {
			d = m.Wall - s.Offset // never answered: run to the end
		}
		s.Width = max(pct(float64(d)/total), 0.5)
		if s.Left+s.Width > 100 {
			s.Width = 100 - s.Left
		}
	}
	return m
}

func pct(f float64) float64 {
	return min(max(f*100, 0), 100)
}

func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// userBlocks decodes a user message's content blocks; string content (a plain
// prompt echo) yields none.
func userBlocks(raw json.RawMessage) []claudecli.ContentBlock {
	var msg struct {
		Content []claudecli.ContentBlock `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return nil
	}
	return msg.Content
}

// flattenContent renders tool_result content, which is a string or an array of
// {type:text,text} parts.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var out []string
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				out = append(out, p.Text)
			} else {
				out = append(out, "["+p.Type+"]")
			}
		}
		return strings.Join(out, "\n")
	}
	return string(raw)
}

// Load reads path (a recording file or a directory of *.jsonl recordings) and
// returns the models sorted by start time, plus the contents of graph.mmd when
// a directory holds one. A single file that is not a recording is an error; in
// a directory such files are skipped, but at least one recording is required.
func Load(path string) (models []Model, graph string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", err
	}
	if !st.IsDir() {
		rec, err := recording.ReadFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", path, err)
		}
		m := Build(rec)
		m.Source = filepath.Base(path)
		m.ID = "run-1"
		return []Model{m}, "", nil
	}
	files, err := filepath.Glob(filepath.Join(path, "*.jsonl"))
	if err != nil {
		return nil, "", err
	}
	sort.Strings(files)
	for _, f := range files {
		rec, err := recording.ReadFile(f)
		if err != nil {
			continue
		}
		m := Build(rec)
		m.Source = filepath.Base(f)
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil, "", fmt.Errorf("%s: no recordings (*.jsonl) found", path)
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].StartedAt.Before(models[j].StartedAt) })
	for i := range models {
		models[i].ID = fmt.Sprintf("run-%d", i+1)
	}
	if b, err := os.ReadFile(filepath.Join(path, "graph.mmd")); err == nil {
		graph = string(b)
	}
	return models, graph, nil
}

//go:embed template.html
var pageHTML string

var page = template.Must(template.New("trace").Funcs(template.FuncMap{
	"dur":   fmtDur,
	"inc":   func(i int) int { return i + 1 },
	"cost":  func(f float64) string { return fmt.Sprintf("$%.4f", f) },
	"num":   fmtNum,
	"pos":   func(f float64) string { return fmt.Sprintf("%.2f", f) },
	"total": func(ms []Model) Totals { return totals(ms) },
}).Parse(pageHTML))

// Totals aggregates a set of runs for the summary footer.
type Totals struct {
	Cost  float64
	Tools int
	Wall  time.Duration
}

func totals(ms []Model) Totals {
	var t Totals
	for _, m := range ms {
		t.Cost += m.CostUSD
		t.Tools += m.ToolCalls
		t.Wall += m.Wall
	}
	return t
}

func fmtDur(d time.Duration) string {
	switch {
	case d < 0:
		return "—"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

func fmtNum(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Render writes the HTML page for models. graph, if non-empty, is shown as
// preformatted text at the top.
func Render(w io.Writer, models []Model, graph string) error {
	return page.Execute(w, struct {
		Models []Model
		Graph  string
		Multi  bool
	}{models, graph, len(models) > 1})
}
