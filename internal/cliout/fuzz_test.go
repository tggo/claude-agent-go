package cliout

import (
	"strings"
	"testing"
)

// go test -run '^$' -fuzz '^FuzzRedactTokens$' -fuzztime 30s ./internal/cliout

func FuzzRedactTokens(f *testing.F) {
	f.Add("fatal: unable to access 'https://x-access-token:ghs_AAA@github.com/o/r.git/'")
	f.Add("https://x-access-token:one@h/r then https://x-access-token:two@h/r")
	f.Add("x-access-token:dangling")
	f.Add("x-access-token:x-access-token:@@x-access-token:")
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		out := RedactTokens(in)
		if RedactTokens(out) != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, out, RedactTokens(out))
		}
		// Every terminated token in the output is masked.
		rest := out
		for {
			i := strings.Index(rest, "x-access-token:")
			if i < 0 {
				break
			}
			tail := rest[i+len("x-access-token:"):]
			if at := strings.IndexByte(tail, '@'); at >= 0 && tail[:at] != "***" {
				t.Fatalf("unredacted token %q in %q (input %q)", tail[:at], out, in)
			}
			rest = tail
		}
	})
}
