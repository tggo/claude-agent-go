package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tggo/claude-agent-go/graph"
)

const runsUsage = "usage: cag runs <checkpoint store dir> [runID]"

func runsCmd(args []string) {
	if err := runsRun(args, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, runsUsage)
			os.Exit(2)
		}
		die("runs: %v", err)
	}
}

// runsRun lists the runs of a graph.FileStore dir, or, with a run id, prints
// that run's log as a timeline (the seq numbers are what Runnable.Fork takes).
func runsRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, runsUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return usageError{}
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	store, err := graph.FileStore(dir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if fs.NArg() == 2 {
		recs, err := store.Load(ctx, fs.Arg(1))
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return fmt.Errorf("run %q not found in %s", fs.Arg(1), dir)
		}
		fmt.Fprintln(tw, "SEQ\tTIME\tKIND\tNODE\tSTEP\tDETAIL")
		for _, r := range recs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.Seq, r.Time.Local().Format("15:04:05.000"), r.Kind, r.Node, stepStr(r.Step), detail(r))
		}
		return tw.Flush()
	}
	runs, err := store.List(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "no runs in", dir)
		return nil
	}
	fmt.Fprintln(tw, "RUN\tSTATUS\tLAST NODE\tNEXT\tCOMMITS\tCOST\tUPDATED")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t$%.4f\t%s\n", r.RunID, r.Status, dash(r.LastNode), dash(r.Next), r.Commits, r.CostUSD, ago(r.Updated))
	}
	return tw.Flush()
}

func stepStr(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func detail(r graph.Record) string {
	var p []string
	if r.Next != "" {
		p = append(p, "next="+r.Next)
	}
	if r.Kind == graph.KindEdge && r.From != "" {
		p = append(p, "from="+r.From)
	}
	if r.CostUSD > 0 {
		p = append(p, fmt.Sprintf("cost=$%.4f", r.CostUSD))
	}
	if r.DurationMS > 0 {
		p = append(p, fmt.Sprintf("took=%dms", r.DurationMS))
	}
	if r.SessionID != "" {
		p = append(p, "session="+r.SessionID)
	}
	if r.Question != "" {
		p = append(p, fmt.Sprintf("question=%q", r.Question))
	}
	if r.Answer != "" {
		p = append(p, fmt.Sprintf("answer=%q", r.Answer))
	}
	if r.Kind == graph.KindFork {
		p = append(p, fmt.Sprintf("from=%s@%d", r.From, r.FromSeq))
	}
	if r.Err != "" {
		p = append(p, "err="+r.Err)
	}
	return strings.Join(p, " ")
}
