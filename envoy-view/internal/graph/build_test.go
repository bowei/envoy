package graph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/boweidu/envoy-view/internal/xds"
)

func load(t *testing.T, fixture string, opts Options) *Graph {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "xds", "testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ix, err := xds.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return Build(ix, opts)
}

func (g *Graph) mustNode(t *testing.T, id NodeID) *Node {
	t.Helper()
	n := g.node(id)
	if n == nil {
		t.Fatalf("node %q not in graph; have:\n%s", id, strings.Join(g.nodeIDs(), "\n"))
	}
	return n
}

func (g *Graph) nodeIDs() []string {
	out := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = string(n.ID)
	}
	return out
}

// targets returns the labels and destination node IDs of edges leaving from.
func (g *Graph) targets(from NodeID) map[NodeID]string {
	out := map[NodeID]string{}
	for _, e := range g.Edges {
		if e.From == from {
			out[e.To] = e.Label
		}
	}
	return out
}

func (g *Graph) hasEdge(from, to NodeID) bool {
	_, ok := g.targets(from)[to]
	return ok
}

// A static listener with an inline route config must chain all the way through
// to the cluster's inline endpoints.
func TestBuildStaticPipeline(t *testing.T) {
	g := load(t, "static_basic.json", Options{})

	if len(g.Roots) != 1 || g.Roots[0] != "listener/ingress_http" {
		t.Fatalf("roots = %v", g.Roots)
	}

	l := g.mustNode(t, "listener/ingress_http")
	if l.Sublabel != "0.0.0.0:8080" {
		t.Errorf("listener sublabel = %q, want the bind address", l.Sublabel)
	}

	chain := NodeID("listener/ingress_http/fc/0")
	g.mustNode(t, chain)
	if !g.hasEdge("listener/ingress_http", chain) {
		t.Error("listener is not linked to its filter chain")
	}

	hcm := g.mustNode(t, chain+"/nf/0")
	if hcm.Label != "HTTP connection manager" {
		t.Errorf("network filter label = %q", hcm.Label)
	}
	if !g.hasEdge(chain, hcm.ID) {
		t.Error("filter chain is not linked to its first network filter")
	}

	// The router is the terminal HTTP filter and is what selects the route.
	router := g.mustNode(t, hcm.ID+"/hf/0")
	if router.Label != "envoy.filters.http.router" {
		t.Errorf("http filter label = %q", router.Label)
	}
	if !g.hasEdge(hcm.ID, router.ID) {
		t.Error("HCM is not linked to its first HTTP filter")
	}

	route := g.mustNode(t, hcm.ID+"/inline-route/local_route")
	if !g.hasEdge(router.ID, route.ID) {
		t.Error("terminal HTTP filter is not linked to the route config")
	}
	if route.Details[0].Value != "inline" {
		t.Errorf("route source = %q, want inline", route.Details[0].Value)
	}

	vh := g.mustNode(t, route.ID+"/vh/0")
	r0 := g.mustNode(t, vh.ID+"/r/0")
	if r0.Sublabel != "prefix /" {
		t.Errorf("route match = %q", r0.Sublabel)
	}

	cluster := g.mustNode(t, "cluster/service_backend")
	if !g.hasEdge(r0.ID, cluster.ID) {
		t.Error("route is not linked to its cluster")
	}
	if cluster.Sublabel != "STRICT_DNS" {
		t.Errorf("cluster discovery type = %q", cluster.Sublabel)
	}

	eps := g.mustNode(t, cluster.ID+"/endpoints")
	if eps.Sublabel != "2/2 healthy" {
		t.Errorf("endpoints sublabel = %q, want 2/2 healthy", eps.Sublabel)
	}

	if len(g.Problems) != 0 {
		t.Errorf("clean config produced problems: %v", g.Problems)
	}
}

// RDS, weighted clusters, and the service_name indirection on EDS.
func TestBuildDynamicPipeline(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	hcm := NodeID("listener/ingress_https/fc/0/nf/0")
	g.mustNode(t, hcm)

	// Three HTTP filters chained in order, ending at the router.
	for i, want := range []string{
		"envoy.filters.http.ext_authz",
		"acme.filters.http.watermark",
		"envoy.filters.http.router",
	} {
		n := g.mustNode(t, NodeID(string(hcm)+"/hf/"+string(rune('0'+i))))
		if n.Label != want {
			t.Errorf("http filter %d = %q, want %q", i, n.Label, want)
		}
	}
	router := NodeID(string(hcm) + "/hf/2")

	// RDS resolves to the shared, name-addressed route config node.
	rc := g.mustNode(t, "route/https_route")
	if !g.hasEdge(router, rc.ID) {
		t.Error("router is not linked to the RDS route config")
	}
	if g.targets(router)[rc.ID] != "rds" {
		t.Error("RDS edge is not labelled")
	}

	canary := g.mustNode(t, "route/https_route/vh/0/r/0")
	if canary.Label != "canary" {
		t.Fatalf("route 0 = %q", canary.Label)
	}
	weights := g.targets(canary.ID)
	if got := weights["cluster/svc_v1"]; got != "80 (80%)" {
		t.Errorf("svc_v1 weight label = %q", got)
	}
	if got := weights["cluster/svc_v2"]; got != "20 (20%)" {
		t.Errorf("svc_v2 weight label = %q", got)
	}

	// svc_v1 declares a service_name, so its endpoints live under that key.
	if !g.hasEdge("cluster/svc_v1", "endpoints/outbound|8080|v1|svc.ns.svc.cluster.local") {
		t.Error("EDS cluster not linked to endpoints keyed by service_name")
	}
	// svc_v2 omits service_name, so the cluster name is the key.
	if !g.hasEdge("cluster/svc_v2", "endpoints/svc_v2") {
		t.Error("EDS cluster not linked to endpoints keyed by cluster name")
	}

	eps := g.mustNode(t, "endpoints/outbound|8080|v1|svc.ns.svc.cluster.local")
	if eps.Sublabel != "1/2 healthy" {
		t.Errorf("endpoints sublabel = %q, want 1/2 healthy", eps.Sublabel)
	}
	if eps.Status != StatusWarning {
		t.Errorf("partially unhealthy endpoint set status = %s, want warning", eps.Status)
	}
}

// A route naming a cluster that is not in the dump is the bug this tool exists
// to make visible, so it must become a node rather than a dropped edge.
func TestBuildDanglingClusterReference(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	legacy := g.mustNode(t, "route/https_route/vh/0/r/1")
	if legacy.Label != "legacy" {
		t.Fatalf("route 1 = %q", legacy.Label)
	}

	missing := g.mustNode(t, "unresolved/cluster/missing_cluster")
	if missing.Kind != NodeUnresolved || missing.Status != StatusError {
		t.Errorf("missing cluster node = %s/%s", missing.Kind, missing.Status)
	}
	if !g.hasEdge(legacy.ID, missing.ID) {
		t.Error("route is not linked to the unresolved cluster")
	}

	var found bool
	for _, p := range g.Problems {
		if p.NodeID == missing.ID && strings.Contains(p.Message, "missing_cluster") {
			found = true
		}
	}
	if !found {
		t.Errorf("dangling reference not reported in Problems: %v", g.Problems)
	}
}

func TestBuildNonForwardingActions(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	health := g.mustNode(t, "route/https_route/vh/0/r/2")
	if got := detailValue(health, "action"); got != "direct response 200" {
		t.Errorf("direct response action = %q", got)
	}
	if n := len(g.targets(health.ID)); n != 0 {
		t.Errorf("direct response route has %d outgoing edges, want 0", n)
	}

	redirect := g.mustNode(t, "route/https_route/vh/1/r/0")
	if got := detailValue(redirect, "action"); got != "redirect" {
		t.Errorf("redirect action = %q", got)
	}
}

func TestBuildTCPProxyAndDefaultChain(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	tcp := g.mustNode(t, "listener/ingress_https/fc/1/nf/0")
	if tcp.Label != "TCP proxy" {
		t.Errorf("label = %q", tcp.Label)
	}
	if !g.hasEdge(tcp.ID, "cluster/tcp_backend") {
		t.Error("TcpProxy not linked to its cluster")
	}

	def := g.mustNode(t, "listener/ingress_https/fc/default")
	if def.Label != "default filter chain" {
		t.Errorf("default chain label = %q", def.Label)
	}
	if !g.hasEdge("listener/ingress_https", def.ID) {
		t.Error("listener not linked to its default filter chain")
	}
	// Its TcpProxy names a cluster that does not exist.
	if !g.hasEdge(def.ID+"/nf/0", "unresolved/cluster/blackhole_cluster") {
		t.Error("default chain's dangling cluster not surfaced")
	}
}

func TestBuildFilterChainMatch(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	fc := g.mustNode(t, "listener/ingress_https/fc/0")
	if !strings.Contains(fc.Sublabel, "api.example.com") {
		t.Errorf("chain sublabel = %q, want the SNI match", fc.Sublabel)
	}
	if got := detailValue(fc, "sni"); got != "api.example.com" {
		t.Errorf("sni detail = %q", got)
	}
	if got := detailValue(fc, "transport"); got != "tls" {
		t.Errorf("transport detail = %q", got)
	}
}

func TestBuildSDSSecretLink(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_https"}})

	if !g.hasEdge("listener/ingress_https/fc/0", "secret/server_cert") {
		t.Error("TLS filter chain not linked to its SDS secret")
	}
	s := g.mustNode(t, "secret/server_cert")
	if s.Kind != NodeSecret || s.Status != StatusOK {
		t.Errorf("secret node = %s/%s", s.Kind, s.Status)
	}
}

// A rejected update means the graph shows older config than the control plane
// last sent, which has to be visible or the graph is quietly lying.
func TestBuildSurfacesRejectedUpdate(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_rejected"}})

	l := g.mustNode(t, "listener/ingress_rejected")
	if l.Status != StatusError {
		t.Errorf("status = %s, want error", l.Status)
	}
	if !containsSubstring(l.Notes, "duplicate filter chain match") {
		t.Errorf("notes = %v, want the NACK reason", l.Notes)
	}
	if len(g.Problems) == 0 {
		t.Error("rejected update not reported in Problems")
	}
}

func TestBuildFlagsWarmingCopy(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"ingress_rollout"}})

	l := g.mustNode(t, "listener/ingress_rollout")
	if l.Status != StatusWarning {
		t.Errorf("status = %s, want warning", l.Status)
	}
	if !containsSubstring(l.Notes, "warming") {
		t.Errorf("notes = %v, want a warming note", l.Notes)
	}
	// The graph shows the active config, not the warming one.
	if !g.hasEdge("listener/ingress_rollout/fc/0/nf/0", "cluster/svc_v1") {
		t.Error("graph does not follow the active listener's cluster")
	}
}

func TestBuildOrphanClusters(t *testing.T) {
	without := load(t, "dynamic_full.json", Options{})
	if without.node("cluster/svc_v3") != nil {
		t.Error("warming, unreferenced cluster appeared without IncludeOrphanClusters")
	}

	g := load(t, "dynamic_full.json", Options{IncludeOrphanClusters: true})
	orphan := g.mustNode(t, "cluster/svc_v3")
	if orphan.Status != StatusWarning {
		t.Errorf("orphan status = %s, want warning", orphan.Status)
	}
	if !containsSubstring(orphan.Notes, "no route") {
		t.Errorf("orphan notes = %v", orphan.Notes)
	}
	// ext_authz_cluster is referenced only from a filter config we do not
	// traverse, so it is an orphan by this definition too.
	g.mustNode(t, "cluster/ext_authz_cluster")
}

// Without include_eds there is no endpoints section at all, and every EDS
// cluster must not then be reported as broken.
func TestBuildWithoutEndpointData(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "xds", "testdata", "dynamic_full.json"))
	if err != nil {
		t.Fatal(err)
	}
	stripped := removeSection(t, raw, "EndpointsConfigDump")

	ix, err := xds.Parse(stripped)
	if err != nil {
		t.Fatal(err)
	}
	g := Build(ix, Options{Roots: []string{"ingress_https"}})

	for _, n := range g.Nodes {
		if n.Kind == NodeUnresolved && strings.Contains(string(n.ID), "endpoint") {
			t.Errorf("EDS cluster reported as broken when endpoints were never collected: %s", n.ID)
		}
	}
	c := g.mustNode(t, "cluster/svc_v1")
	if !containsSubstring(c.Notes, "include_eds") {
		t.Errorf("notes = %v, want a hint about include_eds", c.Notes)
	}
}

func TestBuildUnknownRootIsIgnored(t *testing.T) {
	g := load(t, "dynamic_full.json", Options{Roots: []string{"nope"}})
	if len(g.Nodes) != 0 || len(g.Roots) != 0 {
		t.Errorf("unknown root produced %d nodes", len(g.Nodes))
	}
}

// removeSection drops one top-level config dump section, simulating a dump
// fetched without the query parameter that would have included it.
func removeSection(t *testing.T, raw []byte, typeSuffix string) []byte {
	t.Helper()
	var dump struct {
		Configs []json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil {
		t.Fatal(err)
	}
	kept := dump.Configs[:0]
	for _, c := range dump.Configs {
		var probe struct {
			Type string `json:"@type"`
		}
		if err := json.Unmarshal(c, &probe); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(probe.Type, typeSuffix) {
			kept = append(kept, c)
		}
	}
	dump.Configs = kept
	out, err := json.Marshal(dump)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func detailValue(n *Node, label string) string {
	for _, d := range n.Details {
		if d.Label == label {
			return d.Value
		}
	}
	return ""
}

func containsSubstring(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
