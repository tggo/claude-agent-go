package cliout

import (
	"strings"
	"testing"
)

func TestLineWriter(t *testing.T) {
	var got []string
	w := &LineWriter{Fn: func(s string) { got = append(got, s) }}
	w.Write([]byte("line one\nline "))
	w.Write([]byte("two\nline three\n"))
	w.Write([]byte("partial")) // no newline — buffered, not emitted
	want := []string{"line one", "line two", "line three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

func TestLineWriterReportsFullWrite(t *testing.T) {
	// An io.Writer that under-reports n makes io.MultiWriter fail with
	// ErrShortWrite, which would break the stderr tee.
	w := &LineWriter{Fn: func(string) {}}
	p := []byte("no trailing newline")
	n, err := w.Write(p)
	if n != len(p) || err != nil {
		t.Errorf("Write = (%d, %v), want (%d, nil)", n, err, len(p))
	}
}

func TestRedactTokens(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"tokenized url", "https://x-access-token:ghs_secret@github.com/o/r", "https://x-access-token:***@github.com/o/r"},
		{"no token", "plain output", "plain output"},
		{"token without terminator", "x-access-token:dangling", "x-access-token:dangling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactTokens(tc.in); got != tc.want {
				t.Errorf("RedactTokens(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
