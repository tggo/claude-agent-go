package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/recording"
)

const streamOK = `{"type":"system","subtype":"init","session_id":"sess-r"}
{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"all done"}]}}
{"type":"result","subtype":"success","session_id":"sess-r","result":"all done","total_cost_usd":0.02,"num_turns":2}`

func TestRecorderAndReplay(t *testing.T) {
	bin := fakeClaude(t, streamOK)
	dir := t.TempDir()
	var obs []RunRecord
	r := New(WithBinary(bin), WithRecorder(recording.Dir(dir)),
		WithObserver(observerFunc(func(rr RunRecord) { obs = append(obs, rr) })))

	res, err := r.RunStream(context.Background(), Input{Prompt: "list files", Label: "coder"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "all done" || res.SessionID != "sess-r" {
		t.Fatalf("res = %+v", res)
	}
	if len(obs) != 1 || obs[0].Label != "coder" {
		t.Fatalf("observer label: %+v", obs)
	}

	files, _ := filepath.Glob(filepath.Join(dir, "*-coder.jsonl"))
	if len(files) != 1 {
		t.Fatalf("recordings = %v", files)
	}
	rec, err := recording.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.Header.Label != "coder" || rec.Header.Prompt != "list files" || rec.Header.Model != "sonnet" {
		t.Errorf("header = %+v", rec.Header)
	}
	if len(rec.Lines) != 4 {
		t.Fatalf("lines = %d", len(rec.Lines))
	}

	// Replay the recording through a runner: same result, no claude involved.
	rp := New(WithTransport(recording.ReplayTransport{Path: files[0]}))
	var tools []string
	res2, err := rp.RunStream(context.Background(), Input{Prompt: "ignored"}, func(ev claudecli.StreamEvent, _ int) {
		if am := ev.AssistantMessage(); am != nil {
			for _, b := range am.Content {
				if b.Type == "tool_use" {
					tools = append(tools, b.Name)
				}
			}
		}
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.ResultText != "all done" || res2.TotalCostUSD != 0.02 || res2.NumTurns != 2 {
		t.Errorf("replayed = %+v", res2)
	}
	if strings.Join(tools, ",") != "Bash" {
		t.Errorf("replayed tools = %v", tools)
	}
}

type observerFunc func(RunRecord)

func (f observerFunc) ObserveRun(r RunRecord) { f(r) }

func TestRecorderFailureDoesNotBlockRun(t *testing.T) {
	bin := fakeClaude(t, streamOK)
	r := New(WithBinary(bin), WithRecorder(func(recording.Header) (*recording.Writer, error) {
		return nil, errors.New("no disk")
	}))
	if _, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil); err != nil {
		t.Fatal(err)
	}
}

type brokenWriter struct{ n int }

func (b *brokenWriter) Write(p []byte) (int, error) {
	b.n++
	if b.n > 1 {
		return 0, errors.New("broken")
	}
	return len(p), nil
}

func TestRecorderWriteFailureContinues(t *testing.T) {
	bin := fakeClaude(t, streamOK)
	r := New(WithBinary(bin), WithRecorder(func(h recording.Header) (*recording.Writer, error) {
		return recording.NewWriter(&brokenWriter{}, h)
	}))
	res, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	if err != nil || res.ResultText != "all done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestTruncatedStream(t *testing.T) {
	bin := fakeClaude(t, `{"type":"system","subtype":"init","session_id":"sess-t"}
{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"half an ans"}]}}`)
	r := New(WithBinary(bin))
	res, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	te, ok := IsTruncatedStream(err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	if te.SessionID != "sess-t" || te.Events != 2 || !strings.Contains(te.PartialText, "half an ans") {
		t.Errorf("te = %+v", te)
	}
	if res == nil || res.SessionID != "sess-t" {
		t.Errorf("partial result = %+v", res)
	}
	if SessionIDOf(err) != "sess-t" || !strings.Contains(err.Error(), "without a result event") {
		t.Errorf("SessionIDOf/Error: %v", err)
	}
	if !DefaultRetryable(res, err) {
		t.Error("truncation should be retryable")
	}
}

func TestSessionIDOf(t *testing.T) {
	if SessionIDOf(nil) != "" || SessionIDOf(errors.New("x")) != "" {
		t.Error("plain errors carry no session")
	}
	if SessionIDOf(&ProcessError{SessionID: "p"}) != "p" {
		t.Error("process error")
	}
	if SessionIDOf(&TimeoutError{SessionID: "t"}) != "t" {
		t.Error("timeout error")
	}
	if _, ok := IsTruncatedStream(errors.New("x")); ok {
		t.Error("not truncated")
	}
}

func TestProcessErrorCarriesSessionID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"sess-p\"}'\necho 'connection reset' >&2\nexit 1\n"), 0o755)
	_, err := New(WithBinary(bin)).RunStream(context.Background(), Input{Prompt: "x"}, nil)
	pe, ok := IsProcessError(err)
	if !ok || pe.SessionID != "sess-p" {
		t.Fatalf("err = %v", err)
	}
}

// resumeFake fails the first call mid-stream and succeeds only when invoked
// with --resume sess-1, recording the prompt it got on stdin each time.
func resumeFake(t *testing.T) (bin, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "claude")
	script := `#!/bin/sh
prompt=$(cat)
case " $* " in
  *" --resume sess-1 "*)
    echo "resume|$prompt" >> "` + log + `"
    echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
    echo '{"type":"result","subtype":"success","session_id":"sess-1","result":"finished","total_cost_usd":0.03}'
    ;;
  *)
    echo "fresh|$prompt" >> "` + log + `"
    echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
    echo '{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"partial"}]}}'
    ;;
esac
`
	os.WriteFile(bin, []byte(script), 0o755)
	return bin, log
}

func TestRunStreamWithRetryResumes(t *testing.T) {
	bin, log := resumeFake(t)
	var retries []RetryInfo
	res, err := New(WithBinary(bin)).RunStreamWithRetry(context.Background(), Input{Prompt: "do the task"}, nil,
		RetryPolicy{Resume: true, BaseDelay: time.Millisecond, OnRetry: func(ri RetryInfo) { retries = append(retries, ri) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "finished" || res.Attempts != 2 || res.TotalCostUSD != 0.03 {
		t.Errorf("res = %+v", res)
	}
	if len(retries) != 1 {
		t.Errorf("retries = %d", len(retries))
	}
	calls, _ := os.ReadFile(log)
	want := "fresh|do the task\nresume|" + DefaultResumePrompt + "\n"
	if string(calls) != want {
		t.Errorf("calls:\n%s\nwant:\n%s", calls, want)
	}
}

func TestRunStreamWithRetryWithoutResumeReruns(t *testing.T) {
	bin, log := resumeFake(t)
	_, err := New(WithBinary(bin)).RunStreamWithRetry(context.Background(), Input{Prompt: "do it"}, nil,
		RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond})
	if _, ok := IsTruncatedStream(err); !ok {
		t.Fatalf("err = %v", err)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "fresh|do it\nfresh|do it\n" {
		t.Errorf("calls = %q", calls)
	}
}

func TestRunJSONWithRetryIgnoresResume(t *testing.T) {
	out := `[{"type":"result","session_id":"s","result":"ok","total_cost_usd":0.01}]`
	res, err := New(WithBinary(fakeClaude(t, out))).RunJSONWithRetry(context.Background(), Input{Prompt: "x"}, RetryPolicy{Resume: true})
	if err != nil || res.ResultText != "ok" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestLineTooLongDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// One 300 KB line, then more output: the child must not block on the pipe.
	os.WriteFile(bin, []byte("#!/bin/sh\nhead -c 300000 /dev/zero | tr '\\0' 'x'; echo; echo '{\"type\":\"result\",\"result\":\"ok\"}'\n"), 0o755)
	r := New(WithBinary(bin), WithMaxBufferSize(1000), WithTimeout(20*time.Second))
	start := time.Now()
	_, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("RunStream hung for %v", time.Since(start))
	}
	if err == nil || !strings.Contains(err.Error(), "WithMaxBufferSize") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := IsTruncatedStream(err); ok || DefaultRetryable(nil, err) || IsTimeout(err) {
		t.Errorf("line-too-long must not be truncation/timeout/retryable: %v", err)
	}
}

func TestRecordingAttemptsAndPerms(t *testing.T) {
	bin, _ := resumeFake(t)
	dir := filepath.Join(t.TempDir(), "runs")
	_, err := New(WithBinary(bin), WithRecorder(recording.Dir(dir))).RunStreamWithRetry(context.Background(),
		Input{Prompt: "t", Label: "n"}, nil, RetryPolicy{Resume: true, BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	var attempts []int
	for _, f := range files {
		rec, err := recording.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, rec.Header.Attempt)
		if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
			t.Errorf("%s perm = %v", f, st.Mode().Perm())
		}
	}
	if attempts[0]+attempts[1] != 3 {
		t.Errorf("attempts = %v, want 1 and 2", attempts)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir perm = %v", st.Mode().Perm())
	}
}

func TestReplayRelativeDashPath(t *testing.T) {
	src := filepath.Join(t.TempDir(), "r.jsonl")
	f, _ := os.Create(src)
	w, _ := recording.NewWriter(f, recording.Header{})
	w.WriteLine([]byte(`{"type":"result","session_id":"s","result":"replayed"}`))
	w.Close()
	dir := t.TempDir()
	os.Rename(src, filepath.Join(dir, "-r.jsonl"))
	wd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(wd)
	res, err := New(WithTransport(recording.ReplayTransport{Path: "-r.jsonl"})).RunStream(context.Background(), Input{Prompt: "x"}, nil)
	if err != nil || res.ResultText != "replayed" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}
