package graph

import (
	"fmt"
	"slices"
)

// GroupOpts tunes a group declared with GroupWith.
type GroupOpts struct {
	// Label is the title shown by viewers; it defaults to the group name.
	Label string
	// Collapsed asks viewers to draw the group as a single node until the
	// user expands it.
	Collapsed bool
}

type groupDef struct {
	idx   int
	id    string
	opts  GroupOpts
	nodes []string
}

// Group names a stage: a set of nodes that viewers draw together in one box
// and Mermaid exports as a subgraph. Groups are presentation only; they never
// change how the graph runs. A node belongs to at most one group; a parallel
// node's branches follow it. Problems (empty or duplicate group, unknown or
// twice-grouped node) are reported by Compile.
func (g *Graph[S]) Group(name string, nodes ...string) *Graph[S] {
	return g.GroupWith(name, GroupOpts{}, nodes...)
}

// GroupWith is Group with options.
func (g *Graph[S]) GroupWith(name string, opts GroupOpts, nodes ...string) *Graph[S] {
	switch {
	case name == "":
		g.fail("group with empty name")
		return g
	case len(nodes) == 0:
		g.fail("group %q: no nodes", name)
		return g
	}
	for _, d := range g.groups {
		if d.id == name {
			g.fail("group %q: duplicate name", name)
			return g
		}
	}
	g.groups = append(g.groups, &groupDef{idx: len(g.groups), id: name, opts: opts, nodes: slices.Clone(nodes)})
	return g
}

// bindGroups checks the group declarations against the registered nodes and
// attaches each member to its group. It returns every problem found.
func (g *Graph[S]) bindGroups() []error {
	var errs []error
	owner := map[string]string{}
	for _, d := range g.groups {
		for _, n := range d.nodes {
			switch {
			case n == Start || n == End:
				errs = append(errs, fmt.Errorf("group %q: %q is reserved and cannot be grouped", d.id, n))
			case g.nodes[n] == nil:
				errs = append(errs, fmt.Errorf("group %q: unknown node %q", d.id, n))
			case owner[n] == d.id:
				errs = append(errs, fmt.Errorf("group %q: duplicate node %q", d.id, n))
			case owner[n] != "":
				errs = append(errs, fmt.Errorf("node %q: in two groups (%q and %q)", n, owner[n], d.id))
			default:
				owner[n] = d.id
			}
		}
	}
	if len(errs) == 0 {
		for _, d := range g.groups {
			for _, n := range d.nodes {
				g.nodes[n].group = d
			}
		}
	}
	return errs
}

// TopologyGroup is one declared group of a Topology.
type TopologyGroup struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Nodes     []string `json:"nodes"` // in graph order, including parallel branches
	Collapsed bool     `json:"collapsed,omitempty"`
}

// groupTopology fills Topology.Groups and TopoNode.Group.
func (r *Runnable[S]) groupTopology(t *Topology) {
	var order []*groupDef
	members := map[*groupDef][]string{}
	of := map[string]*groupDef{}
	for _, name := range r.order {
		if d := r.nodes[name].group; d != nil {
			of[name] = d
		}
	}
	for _, name := range r.order { // branches follow their parallel node
		if p := r.nodes[name].par; p != nil && of[name] != nil {
			for _, b := range p.branches {
				if of[b] == nil {
					of[b] = of[name]
				}
			}
		}
	}
	for _, name := range r.order {
		if d := of[name]; d != nil {
			if members[d] == nil {
				order = append(order, d)
			}
			members[d] = append(members[d], name)
		}
	}
	if len(order) == 0 {
		return
	}
	for i := range t.Nodes {
		if d := of[t.Nodes[i].ID]; d != nil {
			t.Nodes[i].Group = d.id
		}
	}
	slices.SortStableFunc(order, func(a, b *groupDef) int { return a.idx - b.idx })
	for _, d := range order {
		label := d.opts.Label
		if label == "" {
			label = d.id
		}
		t.Groups = append(t.Groups, TopologyGroup{ID: d.id, Label: label, Nodes: members[d], Collapsed: d.opts.Collapsed})
	}
}
