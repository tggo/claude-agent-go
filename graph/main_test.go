package graph

import (
	"os"
	"testing"
)

// childMains are the bodies a re-executed test binary runs instead of the
// tests when GRAPH_CHILD names one (the subprocess pattern of the crash
// tests: the parent starts os.Args[0] with that variable set).
var childMains = map[string]func(){}

func TestMain(m *testing.M) {
	if name := os.Getenv("GRAPH_CHILD"); name != "" {
		f, ok := childMains[name]
		if !ok {
			println("graph test: unknown GRAPH_CHILD", name)
			os.Exit(3)
		}
		f()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
