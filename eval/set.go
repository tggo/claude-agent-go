package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tggo/claude-agent-go/recording"
)

// Set is a named collection of eval cases, stored as JSON:
//
//	{
//	  "id": "smoke",
//	  "name": "Smoke tests",
//	  "cases": [
//	    {
//	      "id": "echo-hi",
//	      "prompt": "Use the Bash tool to run `echo hi` and tell me the output",
//	      "expected_tools": [{"name": "Bash", "args": {"command": "echo hi"}}],
//	      "match": "in_order",
//	      "ignore_args": false,
//	      "expected_response": "The output is hi",
//	      "rubric": "States that the command printed hi",
//	      "tags": ["bash"]
//	    }
//	  ]
//	}
//
// Fields: id and prompt are required; match is "exact" (default),
// "in_order" or "any_order". expected_tools null/absent means "do not score
// the trajectory"; [] means "expect no tool calls". expected_response is the
// ROUGE/judge reference, rubric a judge criterion, response_contains and
// response_regex are deterministic response checks; all are optional.
type Set struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Cases []Case `json:"cases"`
}

// Case is one eval case: a prompt plus the expectations for the run.
type Case struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`

	// ExpectedTools is the expected trajectory. nil skips trajectory scoring;
	// an empty non-nil slice expects no tool calls at all.
	ExpectedTools []ToolCall `json:"expected_tools"`
	Match         MatchType  `json:"match"`
	IgnoreArgs    bool       `json:"ignore_args,omitempty"`

	ExpectedResponse string `json:"expected_response,omitempty"`
	Rubric           string `json:"rubric,omitempty"`

	// ResponseContains lists substrings (case-insensitive) the response must
	// contain; ResponseRegex is a regexp it must match.
	ResponseContains []string `json:"response_contains,omitempty"`
	ResponseRegex    string   `json:"response_regex,omitempty"`

	Tags []string `json:"tags,omitempty"`
}

// Validate checks that cases have unique non-empty ids and prompts and that
// regexes compile.
func (s Set) Validate() error {
	seen := map[string]bool{}
	for i, c := range s.Cases {
		switch {
		case c.ID == "":
			return fmt.Errorf("eval: case #%d has no id", i)
		case seen[c.ID]:
			return fmt.Errorf("eval: duplicate case id %q", c.ID)
		case c.Prompt == "":
			return fmt.Errorf("eval: case %q has no prompt", c.ID)
		}
		seen[c.ID] = true
		if _, err := Matches("", c.ResponseRegex); err != nil {
			return fmt.Errorf("eval: case %q response_regex: %w", c.ID, err)
		}
	}
	return nil
}

// LoadSet reads and validates the eval set at path. Unknown fields are
// rejected so typos do not silently disable a check.
func LoadSet(path string) (Set, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Set{}, err
	}
	var s Set
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Set{}, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if err := s.Validate(); err != nil {
		return Set{}, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Save writes the set to path as indented JSON, creating parent directories.
func (s Set) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// CaseFromRecording builds a golden case from a recorded run: the prompt
// comes from the recording header, the expected tools (name and args, not
// results) and expected response from the body. Match is InOrder, since a
// golden trajectory should tolerate extra exploratory calls. It is the
// equivalent of ADK's "add session to eval set".
func CaseFromRecording(rec *recording.Recording, id string) Case {
	run := FromRecording(rec)
	tools := make([]ToolCall, len(run.ToolCalls))
	for i, tc := range run.ToolCalls {
		tools[i] = ToolCall{Name: tc.Name, Args: tc.Args}
	}
	return Case{
		ID:               id,
		Prompt:           rec.Header.Prompt,
		ExpectedTools:    tools,
		Match:            InOrder,
		ExpectedResponse: run.Response,
	}
}
