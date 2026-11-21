package graph

import (
	"maps"
	"slices"
)

// SubgraphSep joins a subgraph's name and its node's name: node "plan" of
// subgraph "coding" is "coding/plan" in the parent graph.
const SubgraphSep = "/"

type subgraphDef[S any] struct {
	name string
	sub  *Graph[S]
	opts GroupOpts
}

// AddSubgraph embeds sub as one step of g under name. Build each workflow
// (coding, research, ...) as its own Graph[S] and put them behind a router:
//
//	g.AddNode("classify", classify).
//		AddSubgraph("coding", codingFlow).
//		AddSubgraph("research", researchFlow).
//		AddEdge(graph.Start, "classify").
//		AddConditionalEdge("classify", route, "coding", "research").
//		AddEdge("coding", graph.End).
//		AddEdge("research", graph.End)
//
// name is used like a node name in edges of g: an edge to name enters sub at
// its Start edge, and the route out of name is taken where sub routes to End.
// Compile copies sub's nodes into g as "name/node" and puts them in a group
// called name (labelled with sub's graph name), so viewers draw each workflow
// as one box that can be collapsed, and a durable run records, pauses and
// recovers inside the subgraph like anywhere else. Groups declared inside sub
// (and its own subgraphs) are folded into that one group.
//
// sub shares g's state type and is read when g is compiled, so the same sub
// can be embedded several times under different names. Its Start must have a
// static edge. Its compile options (store, observer, limits) are not used:
// g's apply.
func (g *Graph[S]) AddSubgraph(name string, sub *Graph[S]) *Graph[S] {
	return g.AddSubgraphWith(name, GroupOpts{}, sub)
}

// AddSubgraphWith is AddSubgraph with options for its group (label,
// collapsed).
func (g *Graph[S]) AddSubgraphWith(name string, opts GroupOpts, sub *Graph[S]) *Graph[S] {
	switch {
	case sub == nil:
		g.fail("subgraph %q: nil graph", name)
		return g
	case sub == g:
		g.fail("subgraph %q: a graph cannot embed itself", name)
		return g
	}
	// The name takes a node slot so duplicates and reserved names are caught
	// like for any node; expand replaces it.
	g.addNode(name, &node[S]{name: name, sub: true})
	g.subs = append(g.subs, &subgraphDef[S]{name: name, sub: sub, opts: opts})
	return g
}

// expand returns a copy of g in which every subgraph is replaced by its
// prefixed nodes, wired into g's edges, and a group. g itself is not changed,
// so Compile can be called again. depth guards against a graph that embeds
// itself through another one.
func (g *Graph[S]) expand(depth int) *Graph[S] {
	if len(g.subs) == 0 {
		return g
	}
	x := &Graph[S]{
		name:    g.name,
		nodes:   maps.Clone(g.nodes),
		order:   slices.Clone(g.order),
		static:  maps.Clone(g.static),
		cond:    maps.Clone(g.cond),
		clone:   g.clone,
		labels:  maps.Clone(g.labels),
		agents:  maps.Clone(g.agents),
		runners: maps.Clone(g.runners),
		watches: slices.Clone(g.watches),
		groups:  make([]*groupDef, len(g.groups)),
		errs:    slices.Clone(g.errs),
	}
	for i, d := range g.groups {
		c := *d
		c.nodes = slices.Clone(d.nodes)
		x.groups[i] = &c
	}
	for _, sd := range g.subs {
		x.inline(sd, depth)
	}
	return x
}

// inline replaces the placeholder node sd.name of x with sd.sub's nodes.
func (x *Graph[S]) inline(sd *subgraphDef[S], depth int) {
	name := sd.name
	if depth > 32 {
		x.fail("subgraph %q: nested more than 32 deep (does a graph embed itself?)", name)
		return
	}
	sub := sd.sub.expand(depth + 1)
	for _, err := range sub.errs {
		x.fail("subgraph %q: %w", name, err)
	}
	p := func(n string) string { return name + SubgraphSep + n }
	entry, ok := sub.static[Start]
	switch {
	case !ok:
		x.fail("subgraph %q: its Start needs a static edge", name)
		return
	case entry == End:
		x.fail("subgraph %q: its Start goes straight to End", name)
		return
	}
	entry = p(entry)

	// The route out of the subgraph is the parent's route out of name.
	exitStatic, hasExitStatic := x.static[name]
	exitCond := x.cond[name]
	delete(x.static, name)
	delete(x.cond, name)

	// Swap the placeholder for the prefixed nodes, in place in the order.
	delete(x.nodes, name)
	at := slices.Index(x.order, name)
	x.order = slices.Delete(x.order, at, at+1)
	var members []string
	for _, n := range sub.order {
		sn := sub.nodes[n]
		cp := &node[S]{name: p(n), fn: sn.fn, agent: sn.agent || sub.agents[n], runner: sub.runners[n]}
		if sn.par != nil {
			cp.par = &parallelSpec[S]{merge: sn.par.merge}
			for _, b := range sn.par.branches {
				cp.par.branches = append(cp.par.branches, p(b))
			}
		}
		if _, dup := x.nodes[cp.name]; dup {
			x.fail("subgraph %q: node %q clashes with a node of %q", name, cp.name, x.name)
			continue
		}
		x.nodes[cp.name] = cp
		x.order = slices.Insert(x.order, at+len(members), cp.name)
		members = append(members, cp.name)
		if cp.agent {
			x.agents[cp.name] = true
		}
		if cp.runner != nil {
			x.runners[cp.name] = cp.runner
		}
	}

	// Edges inside the subgraph, with End replaced by the parent's exit.
	for from, to := range sub.static {
		if from == Start {
			continue
		}
		if to != End {
			x.static[p(from)] = p(to)
			continue
		}
		switch {
		case hasExitStatic:
			x.static[p(from)] = exitStatic
		case exitCond != nil:
			x.cond[p(from)] = exitCond
		}
		// No exit: Compile reports the placeholder's missing edge below.
	}
	for from, c := range sub.cond {
		if from == Start {
			x.fail("subgraph %q: its Start needs a static edge", name)
			continue
		}
		x.cond[p(from)] = exitRoute(c, p, exitStatic, hasExitStatic, exitCond)
	}
	if !hasExitStatic && exitCond == nil {
		x.fail("node %q: no outgoing edge", name)
	}
	for k, l := range sub.labels {
		x.labels[[2]string{prefixEnd(k[0], p), prefixEnd(k[1], p)}] = l
	}
	for _, w := range sub.watches {
		x.watches = append(x.watches, [2]string{p(w[0]), p(w[1])})
	}

	// Edges into the subgraph enter at its first node.
	for from, to := range x.static {
		if to == name {
			x.static[from] = entry
		}
	}
	for from, c := range x.cond {
		if slices.Contains(c.targets, name) {
			x.cond[from] = renameTarget(c, name, entry)
		}
	}
	for k, l := range x.labels {
		switch {
		case k[1] == name:
			delete(x.labels, k)
			x.labels[[2]string{k[0], entry}] = l
		case k[0] == name:
			delete(x.labels, k)
		}
	}
	for i, w := range x.watches {
		if w[1] == name {
			x.watches[i][1] = entry
		}
	}
	for _, other := range x.order {
		if pn := x.nodes[other].par; pn != nil && slices.Contains(pn.branches, name) {
			x.fail("parallel node %q: branch %q is a subgraph (use a node that runs it)", other, name)
		}
	}

	// The subgraph's own groups fold into its group.
	for _, d := range x.groups {
		for i, n := range d.nodes {
			if n == name {
				x.fail("group %q: %q is a subgraph, it is already its own group", d.id, name)
				d.nodes = slices.Delete(d.nodes, i, i+1)
				break
			}
		}
	}
	opts := sd.opts
	if opts.Label == "" {
		opts.Label = sub.name
	}
	if len(members) > 0 {
		x.groups = append(x.groups, &groupDef{idx: len(x.groups), id: name, opts: opts, nodes: members})
	}
}

func prefixEnd(n string, p func(string) string) string {
	if n == Start || n == End {
		return n
	}
	return p(n)
}

// exitRoute prefixes a subgraph's conditional edge and sends its End to the
// parent's route out of the subgraph.
func exitRoute[S any](c *condEdge[S], p func(string) string, exitStatic string, hasExitStatic bool, exitCond *condEdge[S]) *condEdge[S] {
	var targets []string
	toEnd := false
	for _, t := range c.targets {
		if t == End {
			toEnd = true
			continue
		}
		targets = append(targets, p(t))
	}
	switch {
	case !toEnd:
	case hasExitStatic:
		targets = append(targets, exitStatic)
	case exitCond != nil:
		for _, t := range exitCond.targets {
			if !slices.Contains(targets, t) {
				targets = append(targets, t)
			}
		}
	}
	route := c.route
	if route == nil { // Compile reports it
		return &condEdge[S]{targets: targets}
	}
	return &condEdge[S]{targets: targets, route: func(s *S) string {
		t := route(s)
		if t != End {
			return p(t)
		}
		switch {
		case hasExitStatic:
			return exitStatic
		case exitCond != nil:
			return exitCond.route(s)
		}
		return End
	}}
}

// renameTarget makes a conditional edge that routes to old route to new.
func renameTarget[S any](c *condEdge[S], old, new string) *condEdge[S] {
	targets := slices.Clone(c.targets)
	for i, t := range targets {
		if t == old {
			targets[i] = new
		}
	}
	route := c.route
	if route == nil {
		return &condEdge[S]{targets: targets}
	}
	return &condEdge[S]{targets: targets, route: func(s *S) string {
		if t := route(s); t != old {
			return t
		}
		return new
	}}
}
