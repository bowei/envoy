// Package graph turns an indexed Envoy config dump into the pipeline graph the
// UI renders: listeners fanning out through filter chains and routes into
// clusters and their endpoints.
//
// Resolving references is the whole point. A config dump is a flat bag of
// resources that refer to each other by name, and the questions people open one
// to answer -- where does this request go, why is this route dark -- are
// questions about those links. References that do not resolve are therefore
// kept as explicit nodes rather than dropped, because a dangling reference is
// usually the bug being hunted.
package graph

import "github.com/boweidu/envoy-view/internal/xds"

// NodeID uniquely identifies a node within one graph.
type NodeID string

// NodeKind is the type of pipeline stage a node represents.
type NodeKind string

const (
	NodeListener      NodeKind = "listener"
	NodeFilterChain   NodeKind = "filterChain"
	NodeNetworkFilter NodeKind = "networkFilter"
	NodeHTTPFilter    NodeKind = "httpFilter"
	NodeRouteConfig   NodeKind = "routeConfig"
	NodeVirtualHost   NodeKind = "virtualHost"
	NodeRoute         NodeKind = "route"
	NodeCluster       NodeKind = "cluster"
	NodeEndpoints     NodeKind = "endpoints"
	NodeSecret        NodeKind = "secret"

	// NodeUnresolved stands in for a name that nothing in the dump defines.
	NodeUnresolved NodeKind = "unresolved"
)

// Status drives how prominently the UI flags a node or edge.
type Status string

const (
	// StatusOK is a stage that resolved cleanly.
	StatusOK Status = "ok"
	// StatusWarning is a stage worth a second look: a cluster with no healthy
	// endpoints, a listener with a warming update pending.
	StatusWarning Status = "warning"
	// StatusError is a stage that is broken: a dangling reference, a rejected
	// update, a resource that would not decode.
	StatusError Status = "error"
)

// ResourceRef points back at the xDS resource a node was built from, so the UI
// can fetch its raw JSON on demand instead of shipping every resource up front.
type ResourceRef struct {
	Kind xds.Kind `json:"kind"`
	Name string   `json:"name"`
}

// Node is one stage in the pipeline graph.
type Node struct {
	ID       NodeID   `json:"id"`
	Kind     NodeKind `json:"kind"`
	Label    string   `json:"label"`
	Sublabel string   `json:"sublabel,omitempty"`
	Status   Status   `json:"status"`

	// Details are the few fields worth showing on the node face itself,
	// in display order.
	Details []Detail `json:"details,omitempty"`

	// Notes explain a non-OK status in words.
	Notes []string `json:"notes,omitempty"`

	// Resource is the backing xDS resource, absent for nodes that are part of
	// a parent resource (filter chains, individual filters, routes).
	Resource *ResourceRef `json:"resource,omitempty"`

	// Collapsed marks a node that stands for more than it shows, such as an
	// endpoint set rendered as a count.
	Collapsed bool `json:"collapsed,omitempty"`
}

// Detail is a labelled value shown on a node face.
type Detail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// EdgeKind distinguishes the two ways stages connect.
type EdgeKind string

const (
	// EdgeChain is sequential flow through a pipeline: filter to next filter.
	EdgeChain EdgeKind = "chain"
	// EdgeRef is a resolved reference by name: a route to its cluster.
	EdgeRef EdgeKind = "ref"
)

// Edge connects two stages.
type Edge struct {
	ID     string   `json:"id"`
	From   NodeID   `json:"from"`
	To     NodeID   `json:"to"`
	Kind   EdgeKind `json:"kind"`
	Label  string   `json:"label,omitempty"`
	Status Status   `json:"status"`
}

// Graph is a renderable pipeline graph.
type Graph struct {
	Nodes []*Node `json:"nodes"`
	Edges []*Edge `json:"edges"`

	// Roots are the listener node IDs the graph was expanded from.
	Roots []NodeID `json:"roots"`

	// Problems collects every dangling reference and rejected update found
	// while building, so the UI can list them without walking the graph.
	Problems []Problem `json:"problems"`
}

// Problem is a resolution failure worth surfacing outside the canvas.
type Problem struct {
	NodeID  NodeID `json:"nodeId"`
	Message string `json:"message"`
	Status  Status `json:"status"`
}

// node returns the node with the given ID, or nil.
func (g *Graph) node(id NodeID) *Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}
