package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/recording"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

func sampleSet() Set {
	return Set{ID: "smoke", Name: "Smoke", Cases: []Case{
		{
			ID: "echo", Prompt: "run echo hi",
			ExpectedTools:    []ToolCall{{Name: "Bash", Args: json.RawMessage(`{"command":"echo hi"}`)}},
			Match:            InOrder,
			ExpectedResponse: "hi", Rubric: "says hi", Tags: []string{"bash"},
			ResponseContains: []string{"hi"}, ResponseRegex: `(?i)hi`,
		},
		{ID: "notools", Prompt: "2+2", ExpectedTools: []ToolCall{}, IgnoreArgs: true},
		{ID: "unscored", Prompt: "anything"},
	}}
}

func TestSetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "set.json")
	in := sampleSet()
	require.NoError(t, in.Save(path))

	raw := readFile(t, path)
	assert.Contains(t, raw, `"match": "in_order"`)
	assert.Contains(t, raw, `"expected_tools": []`, "empty non-nil expectation survives")

	out, err := LoadSet(path)
	require.NoError(t, err)
	assert.Equal(t, in.ID, out.ID)
	require.Len(t, out.Cases, 3)
	assert.Equal(t, InOrder, out.Cases[0].Match)
	assert.JSONEq(t, `{"command":"echo hi"}`, string(out.Cases[0].Args()))
	assert.NotNil(t, out.Cases[1].ExpectedTools)
	assert.Empty(t, out.Cases[1].ExpectedTools)
	assert.Nil(t, out.Cases[2].ExpectedTools, "null expectation stays nil")
	assert.Equal(t, in.Cases[0].Tags, out.Cases[0].Tags)
	assert.True(t, out.Cases[1].IgnoreArgs)
}

// Args is a test helper for the first expected tool's args.
func (c Case) Args() json.RawMessage { return c.ExpectedTools[0].Args }

func TestLoadSetErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
		return p
	}
	_, err := LoadSet(filepath.Join(dir, "nope.json"))
	assert.Error(t, err)

	for name, body := range map[string]string{
		"bad json":      `{`,
		"unknown field": `{"id":"s","cases":[{"id":"a","prompt":"p","exepcted_tools":[]}]}`,
		"unknown match": `{"id":"s","cases":[{"id":"a","prompt":"p","match":"fuzzy"}]}`,
		"no case id":    `{"id":"s","cases":[{"prompt":"p"}]}`,
		"duplicate id":  `{"id":"s","cases":[{"id":"a","prompt":"p"},{"id":"a","prompt":"p"}]}`,
		"no prompt":     `{"id":"s","cases":[{"id":"a"}]}`,
		"bad regex":     `{"id":"s","cases":[{"id":"a","prompt":"p","response_regex":"("}]}`,
	} {
		_, err := LoadSet(write(name+".json", body))
		assert.Error(t, err, name)
	}

	s, err := LoadSet(write("ok.json", `{"id":"s","cases":[{"id":"a","prompt":"p"}]}`))
	require.NoError(t, err)
	assert.Equal(t, Exact, s.Cases[0].Match, "match defaults to exact")
}

func TestSaveError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o644))
	assert.Error(t, Set{ID: "x"}.Save(filepath.Join(blocker, "sub", "s.json")))
}

func TestCaseFromRecording(t *testing.T) {
	rec := buildRecording(t, nil)
	c := CaseFromRecording(rec, "golden-1")
	assert.Equal(t, "golden-1", c.ID)
	assert.Equal(t, "say hi using bash", c.Prompt)
	assert.Equal(t, InOrder, c.Match)
	assert.Equal(t, "The output is hi.", c.ExpectedResponse)
	require.Len(t, c.ExpectedTools, 3)
	assert.Equal(t, "Bash", c.ExpectedTools[0].Name)
	assert.Empty(t, c.ExpectedTools[0].Result, "results are not part of the golden")

	// A golden built from a run must accept that same run.
	run := FromRecording(rec)
	assert.True(t, MatchTrajectory(run.ToolCalls, c.ExpectedTools, c.Match, c.IgnoreArgs))

	// A recording without tools yields an explicit empty expectation.
	empty := CaseFromRecording(&recording.Recording{Header: recording.Header{Prompt: "p"}}, "e")
	assert.NotNil(t, empty.ExpectedTools)
	assert.Empty(t, empty.ExpectedTools)
}
