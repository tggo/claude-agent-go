package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tggo/claude-agent-go/claudecli"
)

// go test -run '^$' -fuzz '^FuzzDefaultRetryable$' -fuzztime 30s ./runner

func FuzzDefaultRetryable(f *testing.F) {
	for _, s := range [][3]string{
		{"", `{"type":"result","is_error":true,"result":"API Error: 529 overloaded"}`, ""},
		{"connection reset by peer", "", ""},
		{"", `{"type":"assistant","message":{"content":[{"type":"text","text":"mentions timeout only"}]}}`, ""},
		{"", `{"type":"result","is_error":true,"api_error_status":429,"result":"x"}`, "Retry-After: 30"},
		{"", `{"type":"result","is_error":true,"api_error_stat`, "retry after 99999999999999999999"},
		{"rate limit\x00", "\n\n{", "retry-after:7"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, stderr, stdout, text string) {
		pe := &ProcessError{ExitCode: 1, Stderr: stderr, Stdout: stdout}
		wrapped := fmt.Errorf("attempt: %w", pe)
		for _, err := range []error{pe, wrapped, errors.New(stderr), nil} {
			_ = DefaultRetryable(nil, err)
			if d := retryAfter(nil, err); d < 0 {
				t.Fatalf("negative retry-after %v", d)
			}
		}
		// Wrapping must not change the classification.
		if DefaultRetryable(nil, pe) != DefaultRetryable(nil, wrapped) {
			t.Fatal("classification differs for wrapped ProcessError")
		}
		// A plain error that is not a ProcessError/Timeout/Truncated is never retried.
		if DefaultRetryable(nil, errors.New(stderr)) {
			t.Fatalf("plain error retried: %q", stderr)
		}
		// A successful, non-error result is never retried whatever the text says.
		if DefaultRetryable(&Result{ResultText: text, Metadata: &claudecli.ExecutionMetadata{Subtype: stderr}}, nil) {
			t.Fatal("non-error result retried")
		}
		res := &Result{IsError: true, ResultText: text, Metadata: &claudecli.ExecutionMetadata{APIErrorStatus: []byte(stdout), Subtype: stderr}}
		_ = DefaultRetryable(res, nil)
		if d := retryAfter(res, pe); d < 0 {
			t.Fatalf("negative retry-after %v", d)
		}
	})
}

func FuzzSanitizeOutput(f *testing.F) {
	f.Add([]byte("clone https://x-access-token:ghs_1@github.com/a/b failed"))
	f.Add([]byte(strings.Repeat("é", 3000)))
	f.Add([]byte("x" + strings.Repeat("é", 3000)))
	f.Add([]byte("\xff\xfe" + strings.Repeat("x", 2500)))
	f.Fuzz(func(t *testing.T, b []byte) {
		for name, s := range map[string]string{"head": sanitizeOutput(b), "tail": sanitizeTail(b)} {
			if len(s) > maxSnippet+32 {
				t.Fatalf("%s: %d bytes exceeds bound", name, len(s))
			}
			if utf8.Valid(b) && !utf8.ValidString(s) {
				t.Fatalf("%s: truncation split a UTF-8 sequence: %q", name, s[:min(len(s), 40)])
			}
		}
		lines := [][]byte{b, b, b}
		if got := tailBytes(lines, 2); len(got) != 2*len(b)+1 {
			t.Fatalf("tailBytes length %d", len(got))
		}
	})
}
