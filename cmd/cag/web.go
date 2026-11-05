package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/tggo/claude-agent-go/graphweb"
)

const webUsage = "usage: cag web --replay <journal.jsonl | runs dir> [--addr 127.0.0.1:8799] [--open]"

func webCmd(args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := webRun(ctx, args, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(os.Stderr, webUsage)
			os.Exit(2)
		}
		die("web: %v", err)
	}
}

// webRun serves a recorded graph run (a graphweb journal) read-only and blocks
// until ctx ends.
func webRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, webUsage); fs.PrintDefaults() }
	replay := fs.String("replay", "", "journal.jsonl written by graphweb, or a runs dir containing it")
	addr := fs.String("addr", "127.0.0.1:0", "listen address (loopback by default)")
	open := fs.Bool("open", false, "open the page in the default browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *replay == "" && fs.NArg() == 1 {
		*replay = fs.Arg(0)
	}
	if *replay == "" || fs.NArg() > 1 {
		return usageError{}
	}
	srv, err := graphweb.NewReplay(*replay)
	if err != nil {
		return fmt.Errorf("%w (graph journals are written by graphweb.WithJournal; for plain recordings use `cag trace`)", err)
	}
	url, err := srv.Start(ctx, *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "replay: %s  (ctrl-c to stop)\n", url)
	if *open {
		if err := openFile(url); err != nil {
			return fmt.Errorf("open: %w", err)
		}
	}
	<-ctx.Done()
	return nil
}
