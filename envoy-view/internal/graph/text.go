package graph

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteText renders the graph as an indented tree, for CLI inspection and for
// eyeballing what the UI will draw.
//
// The graph is a DAG, not a tree: shared nodes such as a cluster reached from
// several routes appear under each. A node already printed on the current path
// is marked rather than re-expanded, so shared subtrees stay readable and
// cycles cannot loop.
func (g *Graph) WriteText(w io.Writer) error {
	out := make([]*Node, 0, len(g.Nodes))
	byID := make(map[NodeID]*Node, len(g.Nodes))
	for _, n := range g.Nodes {
		byID[n.ID] = n
	}

	adj := make(map[NodeID][]*Edge, len(g.Nodes))
	hasParent := make(map[NodeID]bool, len(g.Nodes))
	for _, e := range g.Edges {
		adj[e.From] = append(adj[e.From], e)
		hasParent[e.To] = true
	}

	// Start from the declared roots, then anything else nothing points at, so
	// orphan clusters are not silently dropped from the text view.
	seen := make(map[NodeID]bool)
	roots := append([]NodeID(nil), g.Roots...)
	for _, id := range roots {
		seen[id] = true
	}
	for _, n := range g.Nodes {
		if !hasParent[n.ID] && !seen[n.ID] {
			roots = append(roots, n.ID)
			seen[n.ID] = true
		}
	}

	printed := make(map[NodeID]bool)
	for _, id := range roots {
		if n := byID[id]; n != nil {
			out = append(out, n)
			if err := g.writeNode(w, byID, adj, n, "", "", map[NodeID]bool{}, printed); err != nil {
				return err
			}
		}
	}

	if len(g.Problems) > 0 {
		fmt.Fprintf(w, "\n%d problem(s):\n", len(g.Problems))
		for _, p := range g.Problems {
			fmt.Fprintf(w, "  [%s] %s\n", p.Status, p.Message)
		}
	}
	return nil
}

func (g *Graph) writeNode(
	w io.Writer,
	byID map[NodeID]*Node,
	adj map[NodeID][]*Edge,
	n *Node,
	indent, edgeLabel string,
	onPath map[NodeID]bool,
	printed map[NodeID]bool,
) error {
	// The status marker lives in a fixed gutter before the indent, so a warning
	// does not shift the tree structure by a character.
	gutter := map[Status]string{StatusOK: "  ", StatusWarning: "! ", StatusError: "x "}[n.Status]

	line := gutter + indent + n.Label
	if edgeLabel != "" {
		line += " [" + edgeLabel + "]"
	}
	if n.Sublabel != "" {
		line += "  (" + n.Sublabel + ")"
	}
	if len(n.Details) > 0 {
		parts := make([]string, len(n.Details))
		for i, d := range n.Details {
			parts[i] = d.Label + "=" + d.Value
		}
		line += "  " + strings.Join(parts, " ")
	}
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}
	for _, note := range n.Notes {
		fmt.Fprintf(w, "  %s  ~ %s\n", indent, note)
	}

	if onPath[n.ID] {
		fmt.Fprintf(w, "  %s  ... (cycle)\n", indent)
		return nil
	}
	edges := adj[n.ID]
	if len(edges) == 0 {
		return nil
	}
	// A node reached a second time is shown as a reference, so a cluster used
	// by twenty routes is expanded once.
	if printed[n.ID] {
		fmt.Fprintf(w, "  %s  -> (shown above)\n", indent)
		return nil
	}
	printed[n.ID] = true

	sorted := append([]*Edge(nil), edges...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].To < sorted[j].To })

	onPath[n.ID] = true
	defer delete(onPath, n.ID)

	for _, e := range sorted {
		child, ok := byID[e.To]
		if !ok {
			continue
		}
		if err := g.writeNode(w, byID, adj, child, indent+"  ", e.Label, onPath, printed); err != nil {
			return err
		}
	}
	return nil
}
