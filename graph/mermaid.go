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
	emit := func(name, ind string) {
		n := r.nodes[name]
		switch {
		case n.par != nil:
			fmt.Fprintf(&b, "%ssubgraph %s[%s]\n%s    direction LR\n", ind, ids[name], q(name+" (parallel)"), ind)
			for _, br := range n.par.branches {
				fmt.Fprintf(&b, "%s    %s__%s[%s]\n", ind, ids[name], ids[br], q(br))
			}
			fmt.Fprintf(&b, "%send\n", ind)
		case r.branches[name]:
			// drawn inside its parallel node(s)
		default:
			fmt.Fprintf(&b, "%s%s[%s]\n", ind, ids[name], q(name))
		}
	}
	topo := r.Topology()
	grouped := map[string]bool{}
	for _, gr := range topo.Groups {
		for _, n := range gr.Nodes {
			grouped[n] = true
		}
	}
	for _, name := range r.order {
		if !grouped[name] {
			emit(name, "    ")
		}
	}
	for _, gr := range topo.Groups {
		fmt.Fprintf(&b, "    subgraph %s[%s]\n", mk("grp_"+gr.ID), q(gr.Label))
		for _, name := range gr.Nodes {
			if !r.branches[name] {
				emit(name, "        ")
			}
		}
		b.WriteString("    end\n")
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
	// A branch node is drawn inside its parallel node as <par>__<branch>; a
	// watch link to or from one points at its first such copy.
	drawn := func(name string) string {
		if !r.branches[name] {
			return ids[name]
		}
		for _, p := range r.order {
			if n := r.nodes[p]; n.par != nil && slices.Contains(n.par.branches, name) {
				return ids[p] + "__" + ids[name]
			}
		}
		return ids[name]
	}
	for _, w := range r.watches {
		if r.nodes[w[0]] != nil && r.nodes[w[1]] != nil && w[0] != w[1] {
			fmt.Fprintf(&b, "    %s -. watches .- %s\n", drawn(w[0]), drawn(w[1]))
		}
	}
	return b.String()
}
