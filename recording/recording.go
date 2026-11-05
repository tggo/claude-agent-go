// Package recording defines the on-disk format for a recorded stream-json run
// and the helpers to write, read and replay it.
//
// A recording is a JSONL file:
//
//	line 1:  {"type":"cag_recording","version":1,"label":...,"prompt":...,...}
//	line 2+: the CLI's raw stream-json lines, each with an extra "_cag_t" field
//	         (milliseconds since the run started) injected as the first key.
//
// The injected field keeps every line valid stream-json — parsers ignore
// unknown keys — so the body can be fed back as if the CLI emitted it (see
// ReplayTransport), while still carrying timing for metrics and trace views.
// One file therefore serves as replay fixture, eval golden and trace source.
package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// HeaderType is the "type" of a recording's first line.
const HeaderType = "cag_recording"

// Version is the current format version.
const Version = 1

// Header is the first line of a recording.
type Header struct {
	Type      string    `json:"type"`
	Version   int       `json:"version"`
	Label     string    `json:"label,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Model     string    `json:"model,omitempty"`
	WorkDir   string    `json:"work_dir,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Attempt is the 1-based try of a retried run (runner.*WithRetry), or 0
	// for a single run. Every attempt is recorded: a failed or truncated
	// attempt's tool calls really happened (with Resume the next attempt
	// continues them rather than repeating them).
	Attempt int `json:"attempt,omitempty"`
}

// Line is one recorded stream-json line.
type Line struct {
	// Offset is the time since the run started when the line was read.
	Offset time.Duration
	// Raw is the line as the CLI emitted it (without the _cag_t field).
	Raw []byte
}

// Event parses the line as a stream-json event (nil if it is not one).
func (l Line) Event() *claudecli.StreamEvent { return claudecli.ParseStreamLine(l.Raw) }

// Recording is a parsed recording file.
type Recording struct {
	Header Header
	Lines  []Line
}

// RawLines returns the stream-json lines without timing, in the shape the
// claudecli helpers (ExtractTextFromStream etc.) accept.
func (r *Recording) RawLines() [][]byte {
	out := make([][]byte, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = l.Raw
	}
	return out
}

// Events returns every line that parses as a stream-json event.
func (r *Recording) Events() []claudecli.StreamEvent {
	var out []claudecli.StreamEvent
	for _, l := range r.Lines {
		if ev := l.Event(); ev != nil {
			out = append(out, *ev)
		}
	}
	return out
}

// Writer writes one recording. It is safe for concurrent use.
type Writer struct {
	mu    sync.Mutex
	w     io.Writer
	c     io.Closer
	start time.Time
	now   func() time.Time
	err   error
}

// NewWriter writes h to w and returns a Writer for the body. If w is also an
// io.Closer, Close closes it. h.Type, h.Version and a zero h.StartedAt are
// filled in.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	h.Type = HeaderType
	h.Version = Version
	if h.StartedAt.IsZero() {
		h.StartedAt = time.Now()
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("write recording header: %w", err)
	}
	rw := &Writer{w: w, start: h.StartedAt, now: time.Now}
	if c, ok := w.(io.Closer); ok {
		rw.c = c
	}
	return rw, nil
}

// WriteLine records one raw stream-json line, stamped with the elapsed time.
// Blank lines are skipped. The first write error is sticky and returned by
// every later call and by Close.
func (w *Writer) WriteLine(raw []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	ms := w.now().Sub(w.start).Milliseconds()
	_, w.err = w.w.Write(append(stamp(raw, ms), '\n'))
	return w.err
}

// Close closes the underlying writer when it is an io.Closer.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.c != nil {
		if err := w.c.Close(); err != nil && w.err == nil {
			w.err = err
		}
		w.c = nil
	}
	return w.err
}

// stamp injects "_cag_t":ms as the first key of a JSON object line. Lines that
// are not objects are kept as-is (they replay verbatim, without timing).
func stamp(raw []byte, ms int64) []byte {
	if len(raw) < 2 || raw[0] != '{' {
		return append([]byte(nil), raw...)
	}
	rest := bytes.TrimSpace(raw[1:])
	sep := ","
	if len(rest) > 0 && rest[0] == '}' {
		sep = ""
	}
	return fmt.Appendf(nil, `{"_cag_t":%d%s%s`, ms, sep, rest)
}

// maxOffsetMS bounds a parsed offset so time.Duration(ms)*time.Millisecond
// cannot overflow.
const maxOffsetMS = int64(math.MaxInt64 / int64(time.Millisecond))

var stampRe = regexp.MustCompile(`^\{"_cag_t":(\d+),?`)

// unstamp removes the injected field, returning the original line and offset.
func unstamp(raw []byte) ([]byte, time.Duration) {
	m := stampRe.FindSubmatchIndex(raw)
	if m == nil {
		return raw, 0
	}
	// An absurd (overflowing) value must not wrap into a negative offset.
	ms, err := strconv.ParseInt(string(raw[m[2]:m[3]]), 10, 64)
	if err != nil || ms > maxOffsetMS {
		ms = 0
	}
	out := append([]byte("{"), raw[m[1]:]...)
	return out, time.Duration(ms) * time.Millisecond
}

// ErrNotRecording is returned by Read when the first line is not a header.
var ErrNotRecording = errors.New("recording: missing cag_recording header")

// Read parses a recording from r.
func Read(r io.Reader) (*Recording, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	rec := &Recording{}
	first := true
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal(raw, &rec.Header); err != nil || rec.Header.Type != HeaderType {
				return nil, ErrNotRecording
			}
			continue
		}
		line, off := unstamp(append([]byte(nil), raw...))
		rec.Lines = append(rec.Lines, Line{Offset: off, Raw: line})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if first {
		return nil, ErrNotRecording
	}
	return rec, nil
}

// ReadFile parses the recording at path.
func ReadFile(path string) (*Recording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// ReplayLines returns the stream-json body of the recording at path exactly as
// the CLI emitted it: header dropped, _cag_t stripped. It is the portable
// building block for replaying without sed (see clitest.FromRecording).
func ReplayLines(path string) ([][]byte, error) {
	rec, err := ReadFile(path)
	if err != nil {
		return nil, err
	}
	return rec.RawLines(), nil
}

// Dir returns a factory that writes each recording to its own file in dir,
// named "<UTC timestamp>-<seq>-<label>.jsonl". Recordings hold prompts and raw
// tool output, which can include secrets, so the directory is created 0700
// and files 0600; existing files are never overwritten. It is the usual argument to
// runner.WithRecorder. The directory is created on first use.
func Dir(dir string) func(Header) (*Writer, error) {
	var mu sync.Mutex
	seq := 0
	return func(h Header) (*Writer, error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if h.StartedAt.IsZero() {
			h.StartedAt = time.Now()
		}
		suffix := ""
		if l := slug(h.Label); l != "" {
			suffix = "-" + l
		}
		// O_EXCL: never truncate an existing recording (another factory or
		// process may use the same dir); bump the sequence until a name is free.
		var f *os.File
		for {
			mu.Lock()
			seq++
			n := seq
			mu.Unlock()
			name := fmt.Sprintf("%s-%03d%s.jsonl", h.StartedAt.UTC().Format("20060102T150405"), n, suffix)
			var err error
			f, err = os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				break
			}
			if !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		w, err := NewWriter(f, h)
		if err != nil {
			f.Close()
			return nil, err
		}
		return w, nil
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
