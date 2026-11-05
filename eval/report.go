package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// String renders the report as WriteText does.
func (r *Report) String() string {
	var b strings.Builder
	_ = r.WriteText(&b)
	return b.String()
}

// WriteText writes a detailed human-readable report, similar to
// `adk eval --print_detailed_results`: per case the verdict, expected versus
// actual tool calls, each metric score against its threshold, and the run
// metrics.
func (r *Report) WriteText(w io.Writer) error {
	ew := &errWriter{w: w}
	title := r.SetID
	if r.SetName != "" {
		title += " (" + r.SetName + ")"
	}
	fmt.Fprintf(ew, "Eval set: %s, %d case(s), %d run(s) each\n\n", title, len(r.Cases), max(r.Repeats, 1))
	for _, cr := range r.Cases {
		cr.writeText(ew)
		fmt.Fprintln(ew)
	}
	fmt.Fprintf(ew, "Summary: %d passed, %d failed, %d errored\n", r.Passed(), r.Failed(), r.Errored())
	return ew.err
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// Verdict is "PASS", "FAIL" or "ERROR".
func (cr CaseResult) Verdict() string {
	switch {
	case cr.Err != nil:
		return "ERROR"
	case cr.Pass:
		return "PASS"
	}
	return "FAIL"
}

// Detail renders one case's full report block (what RunT prints on failure).
func (cr CaseResult) Detail() string {
	var b strings.Builder
	cr.writeText(&b)
	return b.String()
}

func (cr CaseResult) writeText(w io.Writer) {
	fmt.Fprintf(w, "%s  %s\n", cr.Verdict(), cr.Case.ID)
	fmt.Fprintf(w, "  prompt:   %s\n", oneLine(cr.Case.Prompt, 200))
	if cr.Err != nil {
		fmt.Fprintf(w, "  error:    %v\n", cr.Err)
	}
	if cr.Case.ExpectedTools != nil {
		fmt.Fprintf(w, "  expected tools (%s%s): %s\n", cr.Case.Match, ternary(cr.Case.IgnoreArgs, ", ignore args", ""), formatCalls(cr.Case.ExpectedTools))
	}
	for i, run := range cr.Runs {
		label := "actual tools"
		if len(cr.Runs) > 1 {
			label = fmt.Sprintf("actual tools [run %d]", i+1)
		}
		fmt.Fprintf(w, "  %s: %s\n", label, formatCalls(run.ToolCalls))
	}
	if n := len(cr.Runs); n > 0 {
		if cr.Case.ExpectedResponse != "" {
			fmt.Fprintf(w, "  expected response: %s\n", oneLine(cr.Case.ExpectedResponse, 200))
		}
		fmt.Fprintf(w, "  actual response:   %s\n", oneLine(cr.Runs[n-1].Response, 200))
	}
	for _, s := range cr.Scores {
		th := fmt.Sprintf("threshold %.2f", s.Threshold)
		if s.ByVerdict {
			th = "judge verdict"
		}
		fmt.Fprintf(w, "  %-18s %.2f (%s) %s", s.Name, s.Value, th, ternary(s.Pass, "pass", "FAIL"))
		if s.Detail != "" {
			fmt.Fprintf(w, "  %s", oneLine(s.Detail, 200))
		}
		fmt.Fprintln(w)
	}
	if len(cr.Runs) > 0 {
		m := cr.Metrics
		fmt.Fprintf(w, "  metrics: tool_calls=%d inferences=%d turns=%d tokens(in/out/cache)=%d/%d/%d cost=$%.4f\n",
			m.ToolCalls, m.Inferences, m.Turns, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CostUSD)
		fmt.Fprintf(w, "           duration=%s model_time=%s tool_time=%s\n", rd(m.Duration), rd(m.ModelTime), rd(m.ToolTime))
		if cr.JudgeCostUSD > 0 {
			fmt.Fprintf(w, "           judge_cost=$%.4f (not in cost above)\n", cr.JudgeCostUSD)
		}
	}
}

func rd(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

func ternary[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

func formatCalls(calls []ToolCall) string {
	if len(calls) == 0 {
		return "(none)"
	}
	parts := make([]string, len(calls))
	for i, c := range calls {
		args := strings.TrimSpace(string(c.Args))
		var buf strings.Builder
		buf.WriteString(c.Name)
		buf.WriteString("(")
		if args != "" && args != "null" {
			var v any
			if json.Unmarshal(c.Args, &v) == nil {
				if b, err := json.Marshal(v); err == nil {
					args = string(b)
				}
			}
			buf.WriteString(oneLine(args, 120))
		}
		buf.WriteString(")")
		parts[i] = buf.String()
	}
	return strings.Join(parts, " -> ")
}

func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "..."
	}
	return s
}
