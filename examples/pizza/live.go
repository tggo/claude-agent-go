package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
	"github.com/tggo/claude-agent-go/runner"
)

// Printer renders stream events as a readable live log. The same renderer
// drives live runs (as a runner.ProgressFunc) and replays of recordings.
type Printer struct {
	mu  sync.Mutex
	w   io.Writer
	t0  time.Time
	now func() time.Duration
}

// NewPrinter writes to w with timestamps relative to now.
func NewPrinter(w io.Writer) *Printer {
	p := &Printer{w: w, t0: time.Now()}
	p.now = func() time.Duration { return time.Since(p.t0) }
	return p
}

// Progress is the live hook for graph.WithProgress.
func (p *Printer) Progress() runner.ProgressFunc {
	return func(ev claudecli.StreamEvent, _ int) { p.Event(ev) }
}

// Note prints a line from the orchestration layer (graph, human, mocks).
func (p *Printer) Note(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.line("●", fmt.Sprintf(format, a...))
}

func (p *Printer) line(icon, text string) {
	pad := strings.Repeat(" ", 10)
	text = strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+pad)
	fmt.Fprintf(p.w, "%6.1fs %s %s\n", p.now().Seconds(), icon, text)
}

type userMsg struct {
	Content []claudecli.ContentBlock `json:"content"`
}

// Event prints one stream-json event.
func (p *Printer) Event(ev claudecli.StreamEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case ev.Type == "system" && ev.Subtype == "init":
		p.line("▶", "agent session started")
	case ev.IsAssistant():
		m := ev.AssistantMessage()
		if m == nil {
			return
		}
		for _, b := range m.Content {
			switch b.Type {
			case claudecli.BlockText:
				if t := strings.TrimSpace(b.Text); t != "" {
					p.line("💬", t)
				}
			case claudecli.BlockToolUse:
				p.line("🔧", shortTool(b.Name)+" "+clip(string(b.Input), 160))
			}
		}
	case ev.Type == "user":
		var m userMsg
		if json.Unmarshal(ev.Message, &m) != nil {
			return
		}
		for _, b := range m.Content {
			if b.Type == claudecli.BlockToolResult {
				icon := "↳"
				if b.IsError {
					icon = "✗"
				}
				p.line(icon, clip(resultText(b.Content), 200))
			}
		}
	case ev.IsResult():
		p.line("✓", fmt.Sprintf("agent done: %d turns, $%.4f", ev.NumTurns, ev.Cost()))
	}
}

func shortTool(n string) string {
	if i := strings.LastIndex(n, "__"); i >= 0 {
		return n[i+2:]
	}
	return n
}

// resultText flattens a tool_result content (string or [{type:text,text}]).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []claudecli.ContentBlock
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, x := range parts {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return string(raw)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Journal records orchestration notes (questions, answers, placement) next to
// the agent recordings so a replay shows the whole story, not just the agents.
type Journal struct {
	mu sync.Mutex
	f  *os.File
}

type journalLine struct {
	T    int64  `json:"unix_ms"`
	Text string `json:"text"`
}

// OpenJournal creates dir/journal.jsonl.
func OpenJournal(dir string) (*Journal, error) {
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &Journal{f: f}, nil
}

// Note writes and prints.
func (j *Journal) Note(p *Printer, format string, a ...any) {
	text := fmt.Sprintf(format, a...)
	p.Note("%s", text)
	j.mu.Lock()
	defer j.mu.Unlock()
	b, _ := json.Marshal(journalLine{T: time.Now().UnixMilli(), Text: text})
	j.f.Write(append(b, '\n'))
}

// Close closes the file.
func (j *Journal) Close() error { return j.f.Close() }

// Replay plays a runs dir back: agent recordings and journal notes merged on
// one timeline, with the original pauses divided by speed (0 = no pauses).
func Replay(w io.Writer, dir string, speed float64) error {
	type item struct {
		at   time.Time
		ev   *claudecli.StreamEvent
		note string
	}
	var items []item
	var t0 time.Time

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(files)
	var journal string
	for _, f := range files {
		if filepath.Base(f) == "journal.jsonl" {
			journal = f
			continue
		}
		rec, err := recording.ReadFile(f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		start := rec.Header.StartedAt
		if t0.IsZero() || start.Before(t0) {
			t0 = start
		}
		items = append(items, item{at: start, note: "agent " + rec.Header.Label})
		for _, l := range rec.Lines {
			var ev claudecli.StreamEvent
			if json.Unmarshal(l.Raw, &ev) == nil {
				items = append(items, item{at: start.Add(l.Offset), ev: &ev})
			}
		}
	}
	if journal != "" {
		f, err := os.Open(journal)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var jl journalLine
			if json.Unmarshal(sc.Bytes(), &jl) != nil {
				continue
			}
			at := time.UnixMilli(jl.T)
			if t0.IsZero() || at.Before(t0) {
				t0 = at
			}
			items = append(items, item{at: at, note: jl.Text})
		}
	}
	if len(items) == 0 {
		return fmt.Errorf("no recordings in %s", dir)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })

	p := NewPrinter(w)
	var cur time.Duration
	p.now = func() time.Duration { return cur }
	var prev time.Duration
	for _, it := range items {
		cur = it.at.Sub(t0)
		if speed > 0 && cur > prev {
			time.Sleep(time.Duration(float64(cur-prev) / speed))
		}
		prev = cur
		if it.ev != nil {
			p.Event(*it.ev)
		} else {
			p.Note("%s", it.note)
		}
	}
	return nil
}

// LoadDotEnv sets variables from the first .env found walking up from the
// working directory. Already-set variables win, so CI/env overrides work.
func LoadDotEnv() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, ".env")
		if b, err := os.ReadFile(path); err == nil {
			for _, ln := range strings.Split(string(b), "\n") {
				ln = strings.TrimSpace(ln)
				k, v, ok := strings.Cut(ln, "=")
				if !ok || strings.HasPrefix(ln, "#") {
					continue
				}
				k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
				if _, set := os.LookupEnv(k); !set {
					os.Setenv(k, v)
				}
			}
			return path, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}
