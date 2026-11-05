package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/tggo/claude-agent-go/internal/trace"
)

const traceUsage = "usage: cag trace [-o out.html] [--open] <recording.jsonl | dir>"

// openFile opens path in the default viewer; a variable so tests can stub it.
var openFile = func(path string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, path).Start()
}

func traceCmd(args []string) {
	if err := traceRun(args, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, traceUsage)
			os.Exit(2)
		}
		die("trace: %v", err)
	}
}

type usageError struct{}

func (usageError) Error() string { return traceUsage }

// defaultTraceOut is <input>.html for a file, <dir>/trace.html for a directory.
func defaultTraceOut(in string, isDir bool) string {
	if isDir {
		return filepath.Join(in, "trace.html")
	}
	return strings.TrimSuffix(in, filepath.Ext(in)) + ".html"
}

func traceRun(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, traceUsage); fs.PrintDefaults() }
	out := fs.String("o", "", "output HTML path (default <input>.html, or <dir>/trace.html)")
	open := fs.Bool("open", false, "open the page in the default browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError{}
	}
	in := fs.Arg(0)
	models, graph, err := trace.Load(in)
	if err != nil {
		return err
	}
	st, _ := os.Stat(in)
	if *out == "" {
		*out = defaultTraceOut(in, st.IsDir())
	}
	var buf bytes.Buffer
	if err := trace.Render(&buf, models, graph); err != nil {
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(stdout, *out)
	if *open {
		if err := openFile(*out); err != nil {
			return fmt.Errorf("open: %w", err)
		}
	}
	return nil
}
