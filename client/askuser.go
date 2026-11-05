package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// askUserToolName is the CLI's built-in human-question tool.
const askUserToolName = "AskUserQuestion"

// QuestionOption is one selectable answer to a Question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one question the agent asks the user via AskUserQuestion.
type Question struct {
	Question    string           `json:"question"`
	Header      string           `json:"header,omitempty"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
}

// AskUserFunc answers the agent's questions. It returns a map from question
// text to the answer; for a multi-select question join the chosen labels with
// ", ". Free text that is not one of the options is accepted. An error denies
// the AskUserQuestion call and its message is shown to the model.
type AskUserFunc func(ctx context.Context, qs []Question) (map[string]string, error)

// answerAskUser runs OnAskUser against the raw tool input and renders the
// permission response: allow with the original input plus an "answers" object.
// Failures (malformed input, callback error) become a deny.
func (c *Client) answerAskUser(ctx context.Context, input json.RawMessage) map[string]any {
	var orig map[string]json.RawMessage
	if err := json.Unmarshal(input, &orig); err != nil {
		return Deny("malformed AskUserQuestion input: " + err.Error()).toResponseData(input)
	}
	var qs []Question
	if raw, ok := orig["questions"]; ok {
		if err := json.Unmarshal(raw, &qs); err != nil {
			return Deny("malformed AskUserQuestion questions: " + err.Error()).toResponseData(input)
		}
	}
	answers, err := callAskUser(ctx, c.cfg.OnAskUser, qs)
	if err != nil {
		return Deny(err.Error()).toResponseData(input)
	}
	if answers == nil {
		answers = map[string]string{}
	}
	ab, _ := json.Marshal(answers) // map[string]string cannot fail
	orig["answers"] = ab
	updated, _ := json.Marshal(orig) // RawMessage values are valid JSON
	return AllowWithInput(updated).toResponseData(input)
}

// callAskUser runs fn, turning a panic into an error so the call is denied
// (and the CLI answered) rather than crashing the host.
func callAskUser(ctx context.Context, fn AskUserFunc, qs []Question) (answers map[string]string, err error) {
	defer func() {
		if p := recover(); p != nil {
			answers, err = nil, fmt.Errorf("OnAskUser panicked: %v", p)
		}
	}()
	return fn(ctx, qs)
}

// AnswerFromStdin returns an AskUserFunc that prints each question with
// numbered options to w and reads one line per question from r. A number picks
// that option; for multi-select questions several numbers separated by commas
// are joined with ", ". Anything else is taken as free text. EOF before an
// answer is an error.
func AnswerFromStdin(r io.Reader, w io.Writer) AskUserFunc {
	br := bufio.NewReader(r)
	return func(ctx context.Context, qs []Question) (map[string]string, error) {
		out := make(map[string]string, len(qs))
		for _, q := range qs {
			if q.Header != "" {
				fmt.Fprintf(w, "[%s] ", q.Header)
			}
			fmt.Fprintln(w, q.Question)
			for i, o := range q.Options {
				if o.Description != "" {
					fmt.Fprintf(w, "  %d) %s - %s\n", i+1, o.Label, o.Description)
				} else {
					fmt.Fprintf(w, "  %d) %s\n", i+1, o.Label)
				}
			}
			if q.MultiSelect {
				fmt.Fprint(w, "choice (numbers separated by commas, or free text): ")
			} else {
				fmt.Fprint(w, "choice (number, or free text): ")
			}
			line, err := br.ReadString('\n')
			line = strings.TrimSpace(line)
			if line == "" && err != nil {
				return nil, fmt.Errorf("no answer for %q: %w", q.Question, err)
			}
			out[q.Question] = resolveChoice(q, line)
		}
		return out, nil
	}
}

// resolveChoice maps typed input to option labels where every comma-separated
// token is a valid option number; otherwise the input is returned verbatim.
func resolveChoice(q Question, in string) string {
	toks := []string{in}
	if q.MultiSelect {
		toks = strings.Split(in, ",")
	}
	labels := make([]string, 0, len(toks))
	for _, t := range toks {
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil || n < 1 || n > len(q.Options) {
			return in
		}
		labels = append(labels, q.Options[n-1].Label)
	}
	return strings.Join(labels, ", ")
}
