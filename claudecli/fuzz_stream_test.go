package claudecli

import (
	"bytes"
	"testing"
)

// FuzzExtractTextFromStream feeds arbitrary stdout (split into lines exactly
// as the runner's scanner would) through result extraction.
//
//	go test -run '^$' -fuzz '^FuzzExtractTextFromStream$' -fuzztime 30s ./claudecli
func FuzzExtractTextFromStream(f *testing.F) {
	for _, s := range streamSeeds {
		f.Add([]byte(s + "\n" + s))
	}
	f.Add([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}]}}` + "\n" +
		`{"type":"assistant","parent_tool_use_id":"t","message":{"content":[{"type":"text","text":"sub"}]}}` + "\n" +
		`{"type":"result","result":"final","total_cost_usd":0.2}`))
	f.Fuzz(func(t *testing.T, stdout []byte) {
		var lines [][]byte
		for l := range bytes.SplitSeq(stdout, []byte("\n")) {
			lines = append(lines, bytes.TrimSpace(l))
		}
		text, res := ExtractTextFromStream(lines)
		if res != nil {
			if !res.IsResult() {
				t.Fatal("returned a non-result event as result")
			}
			if res.Result != "" && text != res.Result {
				t.Fatalf("result text %q not authoritative (got %q)", res.Result, text)
			}
			_ = res.Cost()
		}
		// Idempotent and order-stable.
		text2, res2 := ExtractTextFromStream(lines)
		if text != text2 || (res == nil) != (res2 == nil) {
			t.Fatal("non-deterministic extraction")
		}
		// Appending junk never changes the answer when a result text exists.
		if res != nil && res.Result != "" {
			if t3, _ := ExtractTextFromStream(append(lines[:len(lines):len(lines)], []byte("garbage"))); t3 != text {
				t.Fatalf("trailing garbage changed text: %q -> %q", text, t3)
			}
		}
	})
}
