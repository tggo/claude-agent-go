package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tggo/claude-agent-go/recording"
)

func makeRec(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	w, err := recording.NewWriter(f, recording.Header{Label: "n"})
	require.NoError(t, err)
	require.NoError(t, w.WriteLine([]byte(`{"type":"result","subtype":"success","result":"hi","total_cost_usd":0.5}`)))
	require.NoError(t, w.Close())
}

func TestDefaultTraceOut(t *testing.T) {
	assert.Equal(t, "/a/b.html", defaultTraceOut("/a/b.jsonl", false))
	assert.Equal(t, "/a/trace.html", defaultTraceOut("/a", true))
}

func TestTraceRunFile(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "r.jsonl")
	makeRec(t, in)
	var out, errb bytes.Buffer
	require.NoError(t, traceRun([]string{in}, &out, &errb))
	want := filepath.Join(dir, "r.html")
	assert.Equal(t, want+"\n", out.String())
	b, err := os.ReadFile(want)
	require.NoError(t, err)
	assert.Contains(t, string(b), "$0.5000")
}

func TestTraceRunDirOpenAndOutFlag(t *testing.T) {
	dir := t.TempDir()
	makeRec(t, filepath.Join(dir, "r.jsonl"))
	var opened string
	old := openFile
	openFile = func(p string) error { opened = p; return nil }
	defer func() { openFile = old }()

	var out bytes.Buffer
	require.NoError(t, traceRun([]string{"--open", dir}, &out, &out))
	assert.Equal(t, filepath.Join(dir, "trace.html"), opened)

	custom := filepath.Join(t.TempDir(), "x.html")
	out.Reset()
	require.NoError(t, traceRun([]string{"-o", custom, dir}, &out, &out))
	assert.Equal(t, custom+"\n", out.String())
	assert.FileExists(t, custom)

	openFile = func(string) error { return errors.New("no browser") }
	assert.ErrorContains(t, traceRun([]string{"--open", dir}, &out, &out), "no browser")
}

func TestTraceRunErrors(t *testing.T) {
	var b bytes.Buffer
	assert.ErrorIs(t, traceRun(nil, &b, &b), usageError{})
	assert.Error(t, traceRun([]string{"--bogus"}, &b, &b))
	assert.Error(t, traceRun([]string{filepath.Join(t.TempDir(), "nope.jsonl")}, &b, &b))
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	require.NoError(t, os.WriteFile(bad, []byte("x\n"), 0o644))
	assert.Error(t, traceRun([]string{bad}, &b, &b))

	in := filepath.Join(t.TempDir(), "r.jsonl")
	makeRec(t, in)
	assert.Error(t, traceRun([]string{"-o", "/nonexistent-dir/x.html", in}, &b, &b))
	assert.Equal(t, traceUsage, usageError{}.Error())
}

func TestTraceRunHelp(t *testing.T) {
	var b bytes.Buffer
	assert.ErrorIs(t, traceRun([]string{"-h"}, &b, &b), flag.ErrHelp)
	assert.Contains(t, b.String(), "usage: cag trace")
}
