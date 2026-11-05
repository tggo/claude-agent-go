package graphweb

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/graph"
)

func readBody(t *testing.T, resp interface{ Read([]byte) (int, error) }) string {
	t.Helper()
	b, err := io.ReadAll(resp)
	require.NoError(t, err)
	return string(b)
}

// The page may load scripts only from the pinned dagre build, and its inline
// script must parse.
func TestPageScriptsPinnedAndParse(t *testing.T) {
	s, err := NewReplay(filepath.Join("testdata", "chain16.jsonl"))
	require.NoError(t, err)
	ts := newTS(t, s)
	resp := do(t, "GET", ts.URL+"/", "", nil)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	page := readBody(t, resp.Body)

	for _, m := range regexp.MustCompile(`<script[^>]*\ssrc="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		assert.Equal(t, "https://cdnjs.cloudflare.com/ajax/libs/dagre/0.8.5/dagre.min.js", m[1])
	}
	for _, m := range regexp.MustCompile(`<link[^>]*\shref="(https?://[^"]+)"`).FindAllStringSubmatch(page, -1) {
		assert.Regexp(t, `^https://fonts\.(googleapis|gstatic)\.com`, m[1])
	}
	assert.Contains(t, page, "</html>")

	inline := regexp.MustCompile(`(?s)<script>\n(.*?)</script>`).FindAllStringSubmatch(page, -1)
	require.NotEmpty(t, inline)
	js := inline[len(inline)-1][1]
	assert.NotRegexp(t, `https?://`, regexp.MustCompile(`'http://www\.w3\.org/2000/svg'`).ReplaceAllString(js, ""), "inline JS must not reference external hosts")
	for _, want := range []string{"layoutBlock", "autoGroups", "graphweb:ui:", "buildMini"} {
		assert.Contains(t, js, want)
	}

	if node, err := exec.LookPath("node"); err == nil {
		f := filepath.Join(t.TempDir(), "page.js")
		require.NoError(t, os.WriteFile(f, []byte(js), 0o600))
		out, err := exec.Command(node, "--check", f).CombinedOutput()
		assert.NoError(t, err, string(out))
	}
}

func TestReplayLongChainJournals(t *testing.T) {
	for name, wantGroups := range map[string]int{"chain16.jsonl": 6, "chain16_nogroups.jsonl": 0} {
		t.Run(name, func(t *testing.T) {
			s, err := NewReplay(filepath.Join("testdata", name))
			require.NoError(t, err)
			ts := newTS(t, s)
			resp := do(t, "GET", ts.URL+"/api/topology", "", nil)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			var topo graph.Topology
			require.NoError(t, json.Unmarshal([]byte(readBody(t, resp.Body)), &topo))
			assert.Len(t, topo.Nodes, 18) // 16 real + start + end
			assert.Len(t, topo.Groups, wantGroups)
			back := 0
			for _, e := range topo.Edges {
				if e.Back {
					back++
				}
			}
			assert.Equal(t, 3, back)
			if wantGroups > 0 {
				grouped := 0
				for _, n := range topo.Nodes {
					if n.Group != "" {
						grouped++
					}
				}
				assert.Equal(t, 16, grouped)
			}
			kinds := map[string]int{}
			for _, ev := range s.Events() {
				kinds[ev.Type]++
			}
			assert.Equal(t, 1, kinds[TypeArtifact])
			assert.Equal(t, 1, kinds[TypeGraphEnd])
			assert.Equal(t, 22, kinds[TypeNodeStart])
		})
	}
}
