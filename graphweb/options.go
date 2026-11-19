package graphweb

import (
	"fmt"
	"strings"
)

// WithTheme sets token values (see tokenRoles in theme.go and the README: bg,
// surface, raised, line, text, muted, faint, accent, ok, warn, bad, info, ui,
// mono) for the light and the dark theme alike. A name may be written with or
// without the leading "--". Unset tokens keep the built-in value, and the
// colours derived from them (edges, bars, tints) follow. New fails on a name
// or value that is not plain CSS: a value may not hold `;`, braces, `<`, `>`,
// a backslash, a newline or a comment.
func WithTheme(tokens map[string]string) Option {
	return func(c *config) {
		clean, err := cleanTokens(tokens)
		if err != nil {
			c.setErr(err)
			return
		}
		if c.theme == nil {
			c.theme = map[string]string{}
		}
		for k, v := range clean {
			c.theme[k] = v
		}
	}
}

// WithFonts sets the font stack of the interface (ui) and of code and numbers
// (mono). An empty string keeps the default, a system font stack: the page
// loads no web font. It is WithTheme for the two font tokens.
func WithFonts(ui, mono string) Option {
	return func(c *config) {
		t := map[string]string{}
		if ui != "" {
			t["ui"] = ui
		}
		if mono != "" {
			t["mono"] = mono
		}
		WithTheme(t)(c)
	}
}

// WithStylesheet appends CSS after the built-in one, in its own <style>
// element, so @import and @font-face work and any rule can be overridden.
// Every `<` is escaped, so the text cannot close the element.
func WithStylesheet(css string) Option { return func(c *config) { c.stylesheet = css } }

// WithThemeMode fixes the theme to "light" or "dark". "auto", the default,
// follows prefers-color-scheme. The page also honours ?theme=light|dark|auto
// in its URL and a data-theme attribute on <html>.
func WithThemeMode(mode string) Option {
	return func(c *config) {
		switch mode {
		case "light", "dark", "auto":
			c.themeMode = mode
		default:
			c.setErr(fmt.Errorf("graphweb: theme mode %q: want light, dark or auto", mode))
		}
	}
}

// WithEmbed makes the page fit a host's frame: no brand, no mode label, a
// slim transparent header. ?embed=1 in the URL does the same for one view.
func WithEmbed() Option { return func(c *config) { c.embed = true } }

// WithBasePath tells the page, and the handler, the URL prefix Handler is
// mounted under, for example "/runs/x/graph/". The page then requests
// "<prefix>api/events" and so on, and Handler serves both with the prefix kept
// (mux.Handle(prefix, srv.Handler())) and with it stripped (http.StripPrefix),
// and redirects the prefix without its trailing slash. Without it the page
// uses URLs relative to itself, which also work under any mount that ends in
// a slash.
func WithBasePath(prefix string) Option {
	return func(c *config) {
		p := strings.TrimSpace(prefix)
		if p == "" || p == "/" {
			c.basePath = ""
			return
		}
		if strings.ContainsAny(p, "?#\"'<> \\") || strings.Contains(p, "//") {
			c.setErr(fmt.Errorf("graphweb: base path %q: a plain URL path is expected", prefix))
			return
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		c.basePath = p
	}
}

func (c *config) setErr(err error) {
	if c.err == nil {
		c.err = err
	}
}
