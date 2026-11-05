// Package readmetest keeps the README's Go snippets syntactically valid.
package readmetest

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type snippet struct {
	line int // 1-based line of the opening fence
	code string
}

// extract returns the ```go fences of a markdown document.
func extract(md string) []snippet {
	var out []snippet
	sc := bufio.NewScanner(strings.NewReader(md))
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	in, n := false, 0
	var cur snippet
	var b strings.Builder
	for sc.Scan() {
		n++
		l := sc.Text()
		switch {
		case !in && strings.HasPrefix(strings.TrimSpace(l), "```go"):
			in, cur = true, snippet{line: n}
			b.Reset()
		case in && strings.HasPrefix(strings.TrimSpace(l), "```"):
			in = false
			cur.code = b.String()
			out = append(out, cur)
		case in:
			b.WriteString(l + "\n")
		}
	}
	return out
}

// elided matches placeholder fragments ("...", "// ...") that are
// intentionally not valid Go.
var elided = regexp.MustCompile(`(?m)^\s*(\.\.\.|…)\s*$|\(\.\.\.\)|\{\s*\.\.\.\s*\}|,\s*\.\.\.\s*[,)]`)

// parses reports whether code is valid Go as a whole file, a list of
// declarations, or a list of statements. It returns the error of the most
// natural reading (file if it has a package clause, else statements).
func parses(code string) error {
	fset := token.NewFileSet()
	if regexp.MustCompile(`(?m)^package\s`).MatchString(code) {
		_, err := parser.ParseFile(fset, "snippet.go", code, parser.AllErrors)
		return err
	}
	if _, err := parser.ParseFile(fset, "snippet.go", "package p\n"+code, parser.AllErrors); err == nil {
		return nil
	}
	_, err := parser.ParseFile(fset, "snippet.go", "package p\nfunc _() {\n"+code+"\n}", parser.AllErrors)
	return err
}

// knownMixed lists snippets that interleave statements and declarations, so
// they are valid Go in neither reading. Fix the README (split the fence) and
// remove the entry; a snippet containing one of these strings is skipped.
var knownMixed = []string{}

func TestReadmeGoSnippetsParse(t *testing.T) {
	path := filepath.Join("..", "..", "README.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("README not found: %v", err)
	}
	snips := extract(string(b))
	if len(snips) == 0 {
		t.Fatal("no go snippets found in README.md")
	}
	checked := 0
	for _, s := range snips {
		name := fmt.Sprintf("README.md:%d", s.line)
		t.Run(name, func(t *testing.T) {
			if elided.MatchString(s.code) {
				t.Skip("fragment with elided parts")
			}
			for _, k := range knownMixed {
				if strings.Contains(s.code, k) {
					t.Skip("known mixed statements+declarations fragment; split the fence in README")
				}
			}
			checked++
			if err := parses(s.code); err != nil {
				t.Errorf("does not parse: %v\n%s", err, s.code)
			}
		})
	}
	t.Logf("%d snippets, %d checked", len(snips), checked)
}

func TestExtractAndParses(t *testing.T) {
	md := "x\n```go\npackage main\nfunc main(){}\n```\n```sh\nls\n```\n```go\nx := 1\n_ = x\n```\n```go\nfunc f() {}\n```\n"
	s := extract(md)
	if len(s) != 3 || s[0].line != 2 || s[1].line != 9 {
		t.Fatalf("extract = %+v", s)
	}
	for _, sn := range s {
		if err := parses(sn.code); err != nil {
			t.Errorf("%q: %v", sn.code, err)
		}
	}
	if parses("x := (") == nil || parses("package p\nfunc {") == nil {
		t.Error("syntax errors must fail")
	}
	if !elided.MatchString("foo(...)") || !elided.MatchString("  ...\n") || elided.MatchString("fmt.Println(a...)") {
		t.Error("elided heuristic")
	}
}
