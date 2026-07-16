package client

import (
	"sync"

	"github.com/tggo/claude-agent-go/internal/cliout"
)

// stderrTailMax bounds the retained stderr tail. A session is long-lived and its
// stderr is unbounded, so only the tail is kept — enough to explain a death,
// small enough to hold for hours.
const stderrTailMax = 8 * 1024

// tailBuffer retains the last stderrTailMax bytes written to it and discards the
// rest. Written by the exec stderr copier, read by whoever inspects the exit —
// hence the mutex.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > stderrTailMax {
		// Slide the tail down; copy handles the overlap.
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-stderrTailMax:]...)
	}
	return len(p), nil
}

// snippet returns the retained stderr, credential-redacted, for use in an error.
func (b *tailBuffer) snippet() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cliout.RedactTokens(string(b.buf))
}
