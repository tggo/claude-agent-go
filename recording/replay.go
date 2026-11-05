package recording

import (
	"context"
	"os/exec"
	"path/filepath"

	"github.com/tggo/claude-agent-go/transport"
)

// ReplayTransport is a transport.Transport that, instead of launching claude,
// prints the body of a recording to stdout exactly as the CLI originally
// emitted it (header dropped, _cag_t stripped) and discards stdin. Plug it into
// runner.WithTransport to run code against a real recorded session — offline,
// free and deterministic.
//
// It needs /bin/sh and sed (macOS, Linux, WSL). Args and WorkDir are ignored:
// the recording, not the flags, decides the output.
type ReplayTransport struct {
	// Path is the recording file.
	Path string
}

// replayScript drops line 1 (the header), strips the injected _cag_t key, then
// drains stdin so the runner's prompt write never sees a broken pipe.
const replayScript = `sed -E '1d; s/^\{"_cag_t":[0-9]+,?/{/' "$1" && cat >/dev/null`

// Command implements transport.Transport.
func (t ReplayTransport) Command(ctx context.Context, _ []string, _ transport.CommandOpts) *exec.Cmd {
	// An absolute path can't start with '-', so sed never reads it as an option.
	path, err := filepath.Abs(t.Path)
	if err != nil {
		path = t.Path
	}
	return exec.Command("/bin/sh", "-c", replayScript, "replay", path)
}
