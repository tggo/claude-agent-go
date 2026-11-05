package graph

import (
	"fmt"
	"slices"
	"strings"
)

var mermaidKeywords = []string{"end", "graph", "subgraph", "flowchart", "style", "class", "click", "default", "direction", "linkstyle", "classdef"}

// Mermaid renders the graph as a Mermaid flowchart. Static edges are solid,
// conditional edges are dashed and labelled with their target, and a parallel
// node is a subgraph holding its branches.
func (r *Runnable[S]) Mermaid() string {
	ids := map[string]string{Start: "__start__", End: "__end__"}
	used := map[string]bool{"__start__": true, "__end__": true}
	mk := func(raw string) string {
		var b strings.Builder
		for _, c := range raw {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
				b.WriteRune(c)
			} else {
				b.WriteByte('_')
			}
		}
		id := b.String()
		if id == "" || slices.Contains(mermaidKeywords, strings.ToLower(id)) {
			id = "n_" + id
		}
		base := id
		for i := 2; used[id]; i++ {
			id = fmt.Sprintf("%s_%d", base, i)
		}
		used[id] = true
		return id
	}
	for _, name := range r.order {
		ids[name] = mk(name)
	}
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, "#quot;") + `"` }

	var b strings.Builder
	b.WriteString("flowchart TD\n")
	fmt.Fprintf(&b, "    %s([%s])\n", ids[Start], q("start"))
	fmt.Fprintf(&b, "    %s([%s])\n", ids[End], q("end"))
	for _, name := range r.order {
		n := r.nodes[name]
		switch {
		case n.par != nil:
			fmt.Fprintf(&b, "    subgraph %s[%s]\n        direction LR\n", ids[name], q(name+" (parallel)"))
			for _, br := range n.par.branches {
				fmt.Fprintf(&b, "        %s__%s[%s]\n", ids[name], ids[br], q(br))
			}
			b.WriteString("    end\n")
		case r.branches[name]:
			// drawn inside its parallel node(s)
		default:
			fmt.Fprintf(&b, "    %s[%s]\n", ids[name], q(name))
		}
	}
	for _, from := range append([]string{Start}, r.order...) {
		if t, ok := r.static[from]; ok {
			fmt.Fprintf(&b, "    %s --> %s\n", ids[from], ids[t])
		}
		if c, ok := r.cond[from]; ok {
			for _, t := range c.targets {
				label := t
				if t == End {
					label = "end"
				}
				fmt.Fprintf(&b, "    %s -.->|%s| %s\n", ids[from], q(label), ids[t])
			}
		}
	}
	return b.String()
}
