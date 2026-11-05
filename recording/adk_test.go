package recording

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// ADK #7286: events with identical timestamps keep their file order.
func TestADK7286ReadPreservesOrderWithEqualTimestamps(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, Header{Label: "ord"})
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Now()
	w.start, w.now = fixed, func() time.Time { return fixed } // every line gets _cag_t:0
	const n = 100
	for i := 0; i < n; i++ {
		if err := w.WriteLine([]byte(fmt.Sprintf(`{"type":"assistant","seq":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"_cag_t":0,`)) {
		t.Fatalf("timestamps not identical: %s", buf.String()[:200])
	}
	rec, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Lines) != n {
		t.Fatalf("lines = %d", len(rec.Lines))
	}
	for i, l := range rec.Lines {
		if want := fmt.Sprintf(`{"type":"assistant","seq":%d}`, i); string(l.Raw) != want || l.Offset != 0 {
			t.Fatalf("line %d = %s (offset %v), want %s", i, l.Raw, l.Offset, want)
		}
	}
	for i, raw := range rec.RawLines() {
		if want := fmt.Sprintf(`{"type":"assistant","seq":%d}`, i); string(raw) != want {
			t.Fatalf("RawLines %d = %s", i, raw)
		}
	}
}
