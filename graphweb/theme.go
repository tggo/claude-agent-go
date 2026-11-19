package graphweb

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
)

// tokenRoles are the colour and font roles of the page. A host sets them with
// WithTheme; the built-in light and dark themes define all of them.
//
//	bg        page background
//	surface   header, side panel, node boxes
//	raised    buttons, inputs, chips: one step above surface
//	line      borders and dividers
//	text      main text
//	muted     secondary text
//	faint     tertiary text, idle markers
//	accent    selection, running, links, primary buttons
//	ok        done, success
//	warn      waiting, paused, attention
//	bad       failed, error
//	info      informational blue (web tools)
//	ui        font stack of the interface
//	mono      font stack of code, numbers, ids
var tokenRoles = []string{"bg", "surface", "raised", "line", "text", "muted", "faint", "accent", "ok", "warn", "bad", "info", "ui", "mono"}

const (
	fontUI   = `system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif`
	fontMono = `ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace`
)

var lightTheme = map[string]string{
	"bg": "#f6f8fa", "surface": "#ffffff", "raised": "#eef2f6", "line": "#d5dde5",
	"text": "#14202b", "muted": "#566676", "faint": "#8896a4",
	"accent": "#0a7ea4", "ok": "#17803f", "warn": "#9a6200", "bad": "#c8372d", "info": "#1f6fd1",
	"ui": fontUI, "mono": fontMono,
}

var darkTheme = map[string]string{
	"bg": "#0a1320", "surface": "#0e1b2a", "raised": "#13263a", "line": "#1c3047",
	"text": "#e7eff6", "muted": "#8499ab", "faint": "#54677a",
	"accent": "#3dd6f5", "ok": "#58d68d", "warn": "#f2c14e", "bad": "#ff6b5c", "info": "#6fb6ff",
	"ui": fontUI, "mono": fontMono,
}

var tokenName = regexp.MustCompile(`^(--)?[a-zA-Z][a-zA-Z0-9-]*$`)

// themeCSS renders the token declarations of one theme in a stable order.
func themeCSS(scheme string, tokens map[string]string) string {
	keys := make([]string, 0, len(tokens))
	for k := range tokens {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "color-scheme:%s;", scheme)
	for _, k := range keys {
		fmt.Fprintf(&b, "--%s:%s;", k, tokens[k])
	}
	return b.String()
}

// cleanTokens validates host-supplied tokens: a token name is letters,
// digits and dashes, and a value may not close the declaration or the
// <style> element (`;`, braces, `<`, comments, newlines).
func cleanTokens(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !tokenName.MatchString(k) {
			return nil, fmt.Errorf("graphweb: theme token %q: use letters, digits and dashes", k)
		}
		if strings.ContainsAny(v, ";{}<>\\\n\r") || strings.Contains(v, "/*") || strings.Contains(v, "*/") {
			return nil, fmt.Errorf("graphweb: theme token %q: value %q has a character that is not allowed (; { } < > \\ newline or a comment)", k, v)
		}
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("graphweb: theme token %q: empty value", k)
		}
		out[strings.TrimPrefix(k, "--")] = strings.TrimSpace(v)
	}
	return out, nil
}

// safeCSS makes a stylesheet safe to put inside a <style> element: every `<`
// becomes the CSS escape \3c , so `</style>` cannot end the element early.
// A `<` is only valid in CSS inside a string or a comment, where the escape
// reads the same in a string and is inert in a comment.
func safeCSS(css string) string { return strings.ReplaceAll(css, "<", `\3c `) }

// themeBlock is the <style> element with the light and dark defaults, the
// forced theme and the host's tokens. The host's tokens use :root:root:root
// so they win over both defaults whatever the order.
func (c *config) themeBlock() string {
	var b strings.Builder
	b.WriteString("<style id=\"graphweb-theme\">\n")
	b.WriteString(":root{" + themeCSS("light", lightTheme) + "}\n")
	b.WriteString("@media (prefers-color-scheme:dark){:root:not([data-theme=\"light\"]){" + themeCSS("dark", darkTheme) + "}}\n")
	b.WriteString(":root[data-theme=\"dark\"]{" + themeCSS("dark", darkTheme) + "}\n")
	if len(c.theme) > 0 {
		var decl strings.Builder
		keys := make([]string, 0, len(c.theme))
		for k := range c.theme {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&decl, "--%s:%s;", k, c.theme[k])
		}
		b.WriteString(":root:root:root{" + decl.String() + "}\n")
	}
	b.WriteString("</style>")
	return b.String()
}

func (c *config) hostCSS() string {
	if c.stylesheet == "" {
		return ""
	}
	return "<style id=\"graphweb-host\">\n" + safeCSS(c.stylesheet) + "\n</style>"
}

// normHost lower-cases a host name and drops a port and IPv6 brackets, the
// same way the Host guard reads the request.
func normHost(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}
