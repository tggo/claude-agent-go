package graphweb

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func replaySrv(t *testing.T, opts ...Option) *Server {
	t.Helper()
	s, err := NewReplay(filepath.Join("testdata", "chain16.jsonl"), opts...)
	require.NoError(t, err)
	return s
}

func page(t *testing.T, s *Server, path string) string {
	t.Helper()
	ts := newTS(t, s)
	resp := do(t, "GET", ts.URL+path, "", nil)
	require.Equal(t, 200, resp.StatusCode)
	return readBody(t, resp.Body)
}

// The built-in sheet uses role tokens only: no palette name and no literal
// colour that a host's theme could not reach.
func TestPageUsesRoleTokensOnly(t *testing.T) {
	html := string(indexHTML)
	for _, old := range []string{"--ink", "--cyan", "--coral", "--amber", "--green", "--disp"} {
		assert.NotContains(t, html, old)
	}
	assert.Empty(t, regexp.MustCompile(`#[0-9a-fA-F]{6}\b|#[0-9a-fA-F]{3}\b`).FindAllString(html[:strings.Index(html, "<script")], -1), "literal colours in the stylesheet")
	assert.NotContains(t, html, "fonts.googleapis.com")
	for _, tok := range tokenRoles {
		assert.Contains(t, lightTheme, tok)
		assert.Contains(t, darkTheme, tok)
	}
}

func TestDefaultThemesInPage(t *testing.T) {
	p := page(t, replaySrv(t), "/")
	assert.Contains(t, p, `<html lang="en" data-theme="auto" data-embed="0">`)
	assert.Contains(t, p, `@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){color-scheme:dark;`)
	assert.Contains(t, p, `:root[data-theme="dark"]{color-scheme:dark;`)
	assert.Contains(t, p, "--bg:#f6f8fa;") // the light default is the base
	assert.NotContains(t, p, ":root:root:root")
}

func TestWithThemeAndFonts(t *testing.T) {
	p := page(t, replaySrv(t,
		WithTheme(map[string]string{"accent": "#ff00aa", "--bg": "rgb(1 2 3)"}),
		WithFonts(`"Inter",sans-serif`, "")), "/")
	assert.Contains(t, p, ":root:root:root{--accent:#ff00aa;--bg:rgb(1 2 3);--ui:\"Inter\",sans-serif;}")
	assert.NotContains(t, p, "--mono:;")
}

func TestThemeRejectsInjection(t *testing.T) {
	for _, bad := range []string{"red;}</style><script>x()</script>", "a}b", "</style>", "x/*y*/", "a\nb", "\\3c", " "} {
		_, err := New(WithTheme(map[string]string{"accent": bad}))
		assert.Error(t, err, bad)
		_, err = NewReplay(filepath.Join("testdata", "chain16.jsonl"), WithTheme(map[string]string{"accent": bad}))
		assert.Error(t, err, bad)
	}
	for _, bad := range []string{"a b", "x;y:z", "", "1x", "--"} {
		_, err := New(WithTheme(map[string]string{bad: "red"}))
		assert.Error(t, err, bad)
	}
	_, err := New(WithThemeMode("sepia"))
	assert.Error(t, err)
	_, err = New(WithBasePath(`/a"b/`))
	assert.Error(t, err)
}

func TestStylesheetAppendedAndEscaped(t *testing.T) {
	css := `.x{content:"</style><script>alert(1)</script>"}` + "\n" + `@import url("fonts.css");`
	p := page(t, replaySrv(t, WithStylesheet(css)), "/")
	assert.Equal(t, 1, strings.Count(p, `<style id="graphweb-host">`))
	host := p[strings.Index(p, `<style id="graphweb-host">`):]
	host = host[:strings.Index(host, "</style>\n")+len("</style>")]
	assert.Equal(t, 1, strings.Count(host, "</style>"), "the host sheet cannot close its element early")
	assert.NotContains(t, strings.ToLower(host), "<script")
	assert.Contains(t, host, `\3c /style>`)
	assert.Contains(t, host, `@import url("fonts.css");`)
	// appended after the built-in sheet
	assert.Greater(t, strings.Index(p, `<style id="graphweb-host">`), strings.Index(p, "html[data-embed"))
}

func TestThemeModeAndEmbed(t *testing.T) {
	p := page(t, replaySrv(t, WithThemeMode("dark"), WithEmbed()), "/")
	assert.Contains(t, p, `<html lang="en" data-theme="dark" data-embed="1">`)
	p = page(t, replaySrv(t, WithThemeMode("auto")), "/")
	assert.Contains(t, p, `data-theme="auto" data-embed="0"`)
}

func TestPageURLsAreRelativeOrBased(t *testing.T) {
	html := string(indexHTML)
	assert.NotRegexp(t, `['"\x60]/api/`, html, "no absolute /api URL in the page")
	assert.Contains(t, html, "const API = p =>")
	p := page(t, replaySrv(t), "/")
	assert.Contains(t, p, `"base":""`)
	p = page(t, replaySrv(t, WithBasePath("runs/x/graph")), "/runs/x/graph/")
	assert.Contains(t, p, `"base":"/runs/x/graph/"`)
}

func TestBasePathMounts(t *testing.T) {
	s := replaySrv(t, WithBasePath("/runs/x/graph/"))
	ts := newTS(t, s)
	// prefix kept
	assert.Equal(t, 200, do(t, "GET", ts.URL+"/runs/x/graph/", "", nil).StatusCode)
	resp := do(t, "GET", ts.URL+"/runs/x/graph/api/topology", "", nil)
	assert.Equal(t, 200, resp.StatusCode)
	// prefix stripped by the host: the same handler still answers
	assert.Equal(t, 200, do(t, "GET", ts.URL+"/", "", nil).StatusCode)
	assert.Equal(t, 200, do(t, "GET", ts.URL+"/api/topology", "", nil).StatusCode)
	// without the trailing slash: redirect to the page
	nr := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	rr, err := nr.Get(ts.URL + "/runs/x/graph")
	require.NoError(t, err)
	rr.Body.Close()
	assert.Equal(t, 301, rr.StatusCode)
	assert.Equal(t, "/runs/x/graph/", rr.Header.Get("Location"))
	// artifacts and the event stream live under the prefix too
	assert.Equal(t, 404, do(t, "GET", ts.URL+"/runs/x/graph/api/artifact/9999", "", nil).StatusCode)
	// replay stays read-only under a prefix
	resp = do(t, "POST", ts.URL+"/runs/x/graph/api/control", `{"action":"pause"}`, map[string]string{"X-Graphweb-Token": s.Token()})
	assert.Equal(t, 403, resp.StatusCode)
}

func TestAllowedLANHost(t *testing.T) {
	s := replaySrv(t, WithAllowedHosts("192.168.88.210", "Runs.Example.LAN:8080"))
	ts := newTS(t, s)
	for host, want := range map[string]int{
		"192.168.88.210:8090": 200,
		"192.168.88.210":      200,
		"runs.example.lan":    200,
		"RUNS.example.lan:99": 200,
		"192.168.88.211:8090": 403,
		"evil.example":        403,
	} {
		assert.Equal(t, want, do(t, "GET", ts.URL+"/", "", map[string]string{"Host": host}).StatusCode, host)
	}
}

func TestQueryOverridesInPage(t *testing.T) {
	html := string(indexHTML)
	assert.Contains(t, html, "q.get('theme')")
	assert.Contains(t, html, "q.get('embed')==='1'")
}
