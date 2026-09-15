package envoytest_test

import (
	"strings"

	"github.com/boweidu/envoy-view/internal/graph"
)

// Lookups shared by the scenario files. They live here rather than being
// repeated per file because several scenarios independently arrived at the
// same two, and a graph node or edge lookup is not worth a second opinion.

// node returns the node with id, or nil. Callers assert on nil themselves so
// that the failure message can say which scenario expected the node.
func node(g *graph.Graph, id graph.NodeID) *graph.Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// edgeBetween returns the edge from -> to, or nil if the two are not linked.
func edgeBetween(g *graph.Graph, from, to graph.NodeID) *graph.Edge {
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

// hasEdge reports whether from links to to, for the scenarios that care only
// that the pipeline connects and not what the edge is labelled.
func hasEdge(g *graph.Graph, from, to graph.NodeID) bool {
	return edgeBetween(g, from, to) != nil
}

// detail returns the value the detail pane would show for label, or "" when
// the builder did not add that row. The empty string is meaningful: several
// scenarios assert a row is absent.
func detail(n *graph.Node, label string) string {
	for _, d := range n.Details {
		if d.Label == label {
			return d.Value
		}
	}
	return ""
}

// hasNote reports whether any of the node's notes contains substr. Notes are
// prose, so scenarios match a fragment rather than the whole sentence.
func hasNote(n *graph.Node, substr string) bool {
	for _, note := range n.Notes {
		if strings.Contains(note, substr) {
			return true
		}
	}
	return false
}

// nodeIDs renders every node ID for a failure message, because "the node is
// missing" is only actionable next to the list of nodes that are present.
func nodeIDs(g *graph.Graph) string {
	ids := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		ids = append(ids, string(n.ID))
	}
	return strings.Join(ids, ", ")
}
