// Package cliout holds the small pieces of CLI-output handling shared by the
// runner and client packages: splitting a byte stream into lines for a
// callback, and redacting credentials before output reaches a log or an error.
package cliout

import (
	"bytes"
	"strings"
)

// LineWriter calls Fn for each complete '\n'-terminated line written to it,
// buffering any partial trailing line until the next write. Fn runs inline on
// whatever goroutine writes, so it must be quick.
type LineWriter struct {
	Fn  func(string)
	buf []byte
}

func (w *LineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.Fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// RedactTokens masks credentials embedded in output. Agents clone over
// tokenized URLs, so the token can surface in git's own error text — which then
// lands in a log or an error string.
func RedactTokens(s string) string {
	if idx := strings.Index(s, "x-access-token:"); idx != -1 {
		if end := strings.Index(s[idx:], "@"); end != -1 {
			s = s[:idx] + "x-access-token:***" + s[idx+end:]
		}
	}
	return s
}
