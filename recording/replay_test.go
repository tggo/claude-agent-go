package recording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/tggo/claude-agent-go/transport"
)

func writeRec(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "r.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(f, Header{Label: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"result","result":"a, \"b\"","session_id":"s"}`,
		`{}`,
		`not json`,
	} {
		if err := w.WriteLine([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReplayLines(t *testing.T) {
	path := writeRec(t)
	got, err := ReplayLines(path)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{
		[]byte(`{"type":"system","subtype":"init","session_id":"s"}`),
		[]byte(`{"type":"result","result":"a, \"b\"","session_id":"s"}`),
		[]byte(`{}`),
		[]byte(`not json`),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if _, err := ReplayLines(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("want error")
	}
}

// The sed-based transport must emit exactly what ReplayLines returns.
func TestReplayTransportMatchesReplayLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh and sed")
	}
	path := writeRec(t)
	cmd := ReplayTransport{Path: path}.Command(context.Background(), []string{"--ignored"}, transport.CommandOpts{})
	cmd.Stdin = bytes.NewBufferString("prompt")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := ReplayLines(path)
	want := append(bytes.Join(lines, []byte("\n")), '\n')
	if !bytes.Equal(out, want) {
		t.Errorf("transport output:\n%s\nwant:\n%s", out, want)
	}
}
