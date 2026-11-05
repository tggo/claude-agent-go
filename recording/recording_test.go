package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nopCloser struct {
	bytes.Buffer
	closed bool
	err    error
}

func (n *nopCloser) Close() error { n.closed = true; return n.err }

func TestWriteReadRoundTrip(t *testing.T) {
	var buf nopCloser
	start := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	w, err := NewWriter(&buf, Header{Label: "plan", Prompt: "hi", Model: "haiku", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	tick := start
	w.now = func() time.Time { tick = tick.Add(150 * time.Millisecond); return tick }

	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{}`,
		`   `,
		`not json`,
		`{"type":"result","result":"done","session_id":"s1","total_cost_usd":0.01}`,
	}
	for _, l := range lines {
		if err := w.WriteLine([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil || !buf.closed {
		t.Fatalf("close: %v closed=%v", err, buf.closed)
	}
	if err := w.Close(); err != nil { // idempotent
		t.Fatal(err)
	}

	rec, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Header.Type != HeaderType || rec.Header.Version != Version || rec.Header.Label != "plan" || !rec.Header.StartedAt.Equal(start) {
		t.Fatalf("header = %+v", rec.Header)
	}
	if len(rec.Lines) != 4 {
		t.Fatalf("lines = %d, want 4 (blank skipped)", len(rec.Lines))
	}
	if got := string(rec.Lines[0].Raw); got != lines[0] {
		t.Errorf("line0 = %s", got)
	}
	if rec.Lines[0].Offset != 150*time.Millisecond || rec.Lines[3].Offset != 600*time.Millisecond {
		t.Errorf("offsets = %v %v", rec.Lines[0].Offset, rec.Lines[3].Offset)
	}
	if string(rec.Lines[1].Raw) != `{}` {
		t.Errorf("empty object = %s", rec.Lines[1].Raw)
	}
	if string(rec.Lines[2].Raw) != `not json` || rec.Lines[2].Offset != 0 {
		t.Errorf("non-object = %s %v", rec.Lines[2].Raw, rec.Lines[2].Offset)
	}
	if evs := rec.Events(); len(evs) != 3 || evs[2].Result != "done" {
		t.Errorf("events = %+v", evs)
	}
	if raw := rec.RawLines(); len(raw) != 4 {
		t.Errorf("raw = %d", len(raw))
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n > 1 {
		return 0, errors.New("disk full")
	}
	return len(p), nil
}

func TestWriterStickyError(t *testing.T) {
	w, err := NewWriter(&failWriter{}, Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteLine([]byte(`{"a":1}`)); err == nil {
		t.Fatal("want error")
	}
	if err := w.WriteLine([]byte(`{"a":2}`)); err == nil || err.Error() != "disk full" {
		t.Fatalf("sticky = %v", err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("close should return sticky error")
	}
	if _, err := NewWriter(&failWriter{n: 1}, Header{}); err == nil {
		t.Fatal("header write error expected")
	}
}

func TestCloseError(t *testing.T) {
	buf := &nopCloser{err: errors.New("boom")}
	w, _ := NewWriter(buf, Header{})
	if err := w.Close(); err == nil {
		t.Fatal("want close error")
	}
}

func TestReadErrors(t *testing.T) {
	for _, in := range []string{"", "\n\n", `{"type":"assistant"}`, "garbage"} {
		if _, err := Read(strings.NewReader(in)); !errors.Is(err, ErrNotRecording) {
			t.Errorf("Read(%q) = %v", in, err)
		}
	}
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file should error")
	}
}

func TestDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	f := Dir(dir)
	w, err := f(Header{Label: "Review Loop / fixer!"})
	if err != nil {
		t.Fatal(err)
	}
	w.WriteLine([]byte(`{"type":"result","result":"ok"}`))
	w.Close()
	w2, err := f(Header{Label: strings.Repeat("x", 80), StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()

	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("files = %d", len(ents))
	}
	if !strings.HasSuffix(ents[0].Name(), "-001-review-loop-fixer.jsonl") {
		t.Errorf("name = %s", ents[0].Name())
	}
	rec, err := ReadFile(filepath.Join(dir, ents[0].Name()))
	if err != nil || len(rec.Lines) != 1 {
		t.Fatalf("read back: %v %+v", err, rec)
	}

	// Unwritable dir.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	if _, err := Dir(filepath.Join(blocker, "sub"))(Header{}); err == nil {
		t.Error("expected mkdir error")
	}
}

func TestDirNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	// Two independent factories (e.g. two processes) on one dir, same second, same label.
	w1, err := Dir(dir)(Header{Label: "a", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	w1.WriteLine([]byte(`{"type":"result","result":"first"}`))
	w1.Close()
	w2, err := Dir(dir)(Header{Label: "a", StartedAt: start})
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("files = %d, want 2", len(ents))
	}
	rec, err := ReadFile(filepath.Join(dir, "20261105T090000-001-a.jsonl"))
	if err != nil || len(rec.Lines) != 1 {
		t.Fatalf("first recording was clobbered: %v %+v", err, rec)
	}
}
