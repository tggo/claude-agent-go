package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// go test -run '^$' -fuzz '^FuzzRead$' -fuzztime 30s ./recording

const goodHeader = `{"type":"cag_recording","version":1,"started_at":"2026-01-01T00:00:00Z"}` + "\n"

func FuzzRead(f *testing.F) {
	for _, s := range []string{
		"", "\n\n", "garbage", "{}", goodHeader,
		goodHeader + `{"_cag_t":12,"type":"result","result":"hi"}` + "\n",
		goodHeader + `{"_cag_t":99999999999999999999999,"type":"x"}` + "\n",
		goodHeader + "not json\n\n{\"_cag_t\":}\n{\"_cag_t\":5\n",
		goodHeader + `{"_cag_t":1}` + "\n" + `{"_cag_t":2,}` + "\n",
		`{"type":"cag_recording"}` + "\r\n" + `{"type":"system"}` + "\r\n",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		rec, err := Read(bytes.NewReader(data))
		if err != nil {
			if rec != nil {
				t.Fatal("non-nil recording with error")
			}
			return
		}
		if rec.Header.Type != HeaderType {
			t.Fatalf("accepted header type %q", rec.Header.Type)
		}
		raws := rec.RawLines()
		if len(raws) != len(rec.Lines) {
			t.Fatal("RawLines length mismatch")
		}
		for _, l := range rec.Lines {
			if l.Offset < 0 {
				t.Fatalf("negative offset %v", l.Offset)
			}
			_ = l.Event()
		}
		_ = rec.Events()
		// ReadFile / ReplayLines agree with Read.
		p := filepath.Join(dir, "r.jsonl")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ReplayLines(p)
		if err != nil {
			t.Fatalf("ReplayLines failed where Read succeeded: %v", err)
		}
		if len(got) != len(raws) {
			t.Fatalf("ReplayLines %d lines, RawLines %d", len(got), len(raws))
		}
		for i := range got {
			if !bytes.Equal(got[i], raws[i]) {
				t.Fatalf("line %d differs: %q vs %q", i, got[i], raws[i])
			}
		}
	})
}

// normalize is what a line is allowed to look like after a round trip: the
// writer trims it, and for objects drops whitespace after the opening brace.
func normalize(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) >= 2 && raw[0] == '{' {
		return append([]byte("{"), bytes.TrimSpace(raw[1:])...)
	}
	return raw
}

func FuzzWriterRoundTrip(f *testing.F) {
	for _, s := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"result","result":"done","total_cost_usd":0.01}`,
		`{}`, `{ }`, `{"_cag_t":7,"type":"x"}`, `{"_cag_t":0}`,
		`[1,2,3]`, `not json at all`, `{broken`, `  {"a":1}  `, `"string"`, `{"a":"é "}`,
	} {
		f.Add(s, s+"x", s)
	}
	f.Fuzz(func(t *testing.T, a, b, c string) {
		var lines [][]byte
		for _, s := range []string{a, b, c} {
			// A raw line cannot contain a newline (it would become two lines).
			s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
			if len(bytes.TrimSpace([]byte(s))) == 0 {
				continue // blank lines are skipped by design
			}
			lines = append(lines, []byte(s))
		}
		var buf bytes.Buffer
		w, err := NewWriter(&buf, Header{Label: a})
		if err != nil {
			t.Fatal(err)
		}
		// Deterministic, strictly advancing clock so offsets must be monotonic.
		base := w.start
		tick := 0
		w.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * 7 * time.Millisecond) }
		for _, l := range lines {
			if err := w.WriteLine(l); err != nil {
				t.Fatal(err)
			}
		}
		rec, err := Read(&buf)
		if err != nil {
			t.Fatalf("Read of own output: %v\n%q", err, buf.String())
		}
		if len(rec.Lines) != len(lines) {
			t.Fatalf("got %d lines, wrote %d", len(rec.Lines), len(lines))
		}
		var prev time.Duration
		for i, l := range rec.Lines {
			if !bytes.Equal(l.Raw, normalize(lines[i])) {
				t.Fatalf("line %d: wrote %q read %q", i, lines[i], l.Raw)
			}
			if len(l.Raw) < 2 || l.Raw[0] != '{' {
				if l.Offset != 0 { // non-object lines carry no timing
					t.Fatalf("line %d is not an object but has offset %v", i, l.Offset)
				}
				continue
			}
			if l.Offset < prev {
				t.Fatalf("offsets not monotonic at %d: %v < %v", i, l.Offset, prev)
			}
			prev = l.Offset
		}
	})
}
