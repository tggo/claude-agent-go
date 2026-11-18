package graphweb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWithHistoryPreloadsAndCopies(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.jsonl")
	if err := os.WriteFile(old, []byte(`{"seq":7,"type":"graph_start","run":"r1"}
{"seq":9,"type":"node_start","run":"r1","node":"a","step":1}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	newJ := filepath.Join(dir, "new.jsonl")
	s, err := New(WithJournal(newJ), WithHistory(old))
	if err != nil {
		t.Fatal(err)
	}
	s.emit(Event{Run: "r1", Type: TypeNodeEnd, Node: "a", Step: 1})
	evs := s.Events()
	if len(evs) != 3 || evs[0].Seq != 1 || evs[1].Seq != 2 || evs[2].Seq != 3 {
		t.Fatalf("want 3 dense events, got %+v", evs)
	}
	if !s.seen["r1"] || s.latest != "r1" {
		t.Fatal("history run must be known (babysitter checks, latest run)")
	}
	s.Close()
	b, _ := os.ReadFile(newJ)
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("new journal must hold history + live, lines = %d", n)
	}
}

func TestWithHistoryMissingFile(t *testing.T) {
	if _, err := New(WithHistory(filepath.Join(t.TempDir(), "nope.jsonl"))); err == nil {
		t.Fatal("want an error")
	}
}
