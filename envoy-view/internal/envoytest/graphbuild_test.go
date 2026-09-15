package envoytest_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// The graph builder is the part of envoy-view a reader actually looks at, and
// everything it knows it learns from resource names that have to line up across
// four xDS subscriptions. Its own tests run on hand-written fixtures, which
// prove the builder agrees with our idea of a dump. These scenarios prove it
// agrees with Envoy's: same builder, same assertions, but the input is whatever
// a real Envoy decided to emit.
//
// The assertions here deliberately walk edges rather than count nodes. A graph
// with every expected node and a broken link renders as a listener that leads
// nowhere, which is precisely the bug these tests exist to catch.

// ------------------------------------------------------------- graph helpers

func mustGraphNode(t *testing.T, g *graph.Graph, id graph.NodeID) *graph.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not in graph; have:\n  %s", id, strings.Join(graphNodeIDs(g), "\n  "))
	return nil
}

func graphNodeIDs(g *graph.Graph) []string {
	out := make([]string, len(g.Nodes))
	for i, n := range g.Nodes {
		out[i] = string(n.ID) + " (" + string(n.Kind) + ")"
	}
	sort.Strings(out)
	return out
}

func graphNodeKind(g *graph.Graph, id graph.NodeID) graph.NodeKind {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n.Kind
		}
	}
	return ""
}

// edgesInto returns every edge that lands on to, which is how a shared resource
// is told apart from a duplicated one.
func edgesInto(g *graph.Graph, to graph.NodeID) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.Edges {
		if e.To == to {
			out = append(out, e)
		}
	}
	return out
}

// reachableFrom returns the nodes a reader could get to by following edges out
// of root, which is the only definition of "this listener's subgraph" that the
// UI's canvas agrees with.
func reachableFrom(g *graph.Graph, root graph.NodeID) map[graph.NodeID]bool {
	seen := map[graph.NodeID]bool{root: true}
	for queue := []graph.NodeID{root}; len(queue) > 0; {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range g.Edges {
			if e.From == cur && !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return seen
}

// pathKinds walks the shortest edge path from one node to another and returns
// the kind of every node along it, including both ends. A nil result means the
// two nodes are not connected at all.
func pathKinds(g *graph.Graph, from, to graph.NodeID) []graph.NodeKind {
	prev := map[graph.NodeID]graph.NodeID{}
	seen := map[graph.NodeID]bool{from: true}
	for queue := []graph.NodeID{from}; len(queue) > 0; {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			var ids []graph.NodeID
			for at := to; ; at = prev[at] {
				ids = append([]graph.NodeID{at}, ids...)
				if at == from {
					break
				}
			}
			kinds := make([]graph.NodeKind, len(ids))
			for i, id := range ids {
				kinds[i] = graphNodeKind(g, id)
			}
			return kinds
		}
		for _, e := range g.Edges {
			if e.From == cur && !seen[e.To] {
				seen[e.To] = true
				prev[e.To] = cur
				queue = append(queue, e.To)
			}
		}
	}
	return nil
}

func sameKinds(a, b []graph.NodeKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkGraphIntegrity asserts the invariants the React Flow canvas depends on
// and never checks: an edge whose endpoint is missing, or two nodes sharing an
// ID, is either a dropped connection or a rendering crash rather than a visible
// error. Every scenario runs this, because the cheapest place to catch it is
// whichever real config first produces it.
func checkGraphIntegrity(t *testing.T, g *graph.Graph) {
	t.Helper()

	nodes := make(map[graph.NodeID]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if nodes[n.ID] {
			t.Errorf("duplicate node ID %q (kind %s, label %q)", n.ID, n.Kind, n.Label)
		}
		nodes[n.ID] = true
		if n.ID == "" {
			t.Errorf("node with empty ID: kind %s, label %q", n.Kind, n.Label)
		}
		if n.Kind == "" || n.Status == "" {
			t.Errorf("node %q has kind %q status %q; both are required by the UI", n.ID, n.Kind, n.Status)
		}
	}

	edges := make(map[string]bool, len(g.Edges))
	for _, e := range g.Edges {
		if edges[e.ID] {
			t.Errorf("duplicate edge ID %q", e.ID)
		}
		edges[e.ID] = true
		if !nodes[e.From] {
			t.Errorf("edge %q starts at %q, which is not a node in the graph", e.ID, e.From)
		}
		if !nodes[e.To] {
			t.Errorf("edge %q ends at %q, which is not a node in the graph", e.ID, e.To)
		}
	}

	// Roots and problems are both rendered as links into the canvas, so a
	// dangling ID there is a dead click rather than a crash -- still wrong.
	for _, id := range g.Roots {
		if !nodes[id] {
			t.Errorf("root %q is not a node in the graph", id)
		}
	}
	for _, p := range g.Problems {
		if p.NodeID != "" && !nodes[p.NodeID] {
			t.Errorf("problem %q points at %q, which is not a node in the graph", p.Message, p.NodeID)
		}
	}
}

// ------------------------------------------------- 1. a whole HTTP pipeline

// The listener arrives over LDS rather than in the bootstrap so the HTTP filter
// chain can hold a filter this binary is not compiled against: only the
// bootstrap is validated against the linked proto types, and a two-filter chain
// is what proves filters are linked in order instead of all hanging off the
// connection manager.
//
// The cluster is EDS with a service_name that differs from the cluster name,
// because that indirection is the one the builder has to get right to find
// endpoints at all, and a dump is the only place it can be checked end to end.
const graphPipelineBootstrap = `{
  "dynamic_resources": {
    "lds_config": {
      "path_config_source": {"path": "/etc/envoy/lds.json"},
      "resource_api_version": "V3"
    }
  },
  "static_resources": {
    "clusters": [{
      "name": "service_backend",
      "type": "EDS",
      "connect_timeout": "1s",
      "eds_cluster_config": {
        "service_name": "backend_eds",
        "eds_config": {
          "path_config_source": {"path": "/etc/envoy/eds.json"},
          "resource_api_version": "V3"
        }
      }
    }]
  }
}`

const graphPipelineListener = `{
  "@type": "type.googleapis.com/envoy.config.listener.v3.Listener",
  "name": "ingress_http",
  "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
  "traffic_direction": "INBOUND",
  "filter_chains": [{
    "name": "http",
    "filters": [{
      "name": "envoy.filters.network.http_connection_manager",
      "typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
        "stat_prefix": "ingress_http",
        "http_filters": [
          {
            "name": "envoy.filters.http.cors",
            "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.cors.v3.Cors"}
          },
          {
            "name": "envoy.filters.http.router",
            "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
          }
        ],
        "route_config": {
          "name": "local_route",
          "virtual_hosts": [{
            "name": "backend",
            "domains": ["*"],
            "routes": [{"match": {"prefix": "/"}, "route": {"cluster": "service_backend"}}]
          }]
        }
      }
    }]
  }]
}`

const graphPipelineEndpoints = `{
  "@type": "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment",
  "cluster_name": "backend_eds",
  "endpoints": [{
    "locality": {"region": "us-east", "zone": "us-east-1a"},
    "lb_endpoints": [
      {"endpoint": {"address": {"socket_address": {"address": "127.0.0.1", "port_value": 8080}}}},
      {"endpoint": {"address": {"socket_address": {"address": "127.0.0.1", "port_value": 8081}}}}
    ]
  }]
}`

// TestGraphHTTPPipelineIsConnectedEndToEnd is the question the tool is opened
// to answer -- where does a request on this listener end up -- asked of a graph
// built from a real dump. Every stage in between is a name lookup that could
// silently fail, so the assertion is on the chain of edges, not on the nodes
// being present somewhere.
func TestGraphHTTPPipelineIsConnectedEndToEnd(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "graph-http-pipeline",
		Bootstrap: graphPipelineBootstrap,
		Files: map[string]string{
			"lds.json": discoveryResponse("1", graphPipelineListener),
			"eds.json": discoveryResponse("1", graphPipelineEndpoints),
		},
	})

	e.WaitFor("the listener and its endpoints", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_http") != nil &&
			ix.Get(xds.KindEndpoint, "backend_eds") != nil
	})

	g := e.Graph(graph.Options{})
	checkGraphIntegrity(t, g)

	if len(g.Roots) != 1 || g.Roots[0] != "listener/ingress_http" {
		t.Fatalf("roots = %v, want just the one listener", g.Roots)
	}
	if len(g.Problems) != 0 {
		t.Errorf("a config Envoy accepted without complaint produced problems: %+v", g.Problems)
	}

	// The endpoints node, not the cluster, is the end of the pipeline: a
	// cluster with no reachable endpoint set is a dead end the UI must show.
	want := []graph.NodeKind{
		graph.NodeListener,
		graph.NodeFilterChain,
		graph.NodeNetworkFilter,
		graph.NodeHTTPFilter,
		graph.NodeHTTPFilter,
		graph.NodeRouteConfig,
		graph.NodeVirtualHost,
		graph.NodeRoute,
		graph.NodeCluster,
		graph.NodeEndpoints,
	}
	got := pathKinds(g, "listener/ingress_http", "endpoints/backend_eds")
	if got == nil {
		t.Fatalf("no path of edges from the listener to its endpoints; graph holds:\n  %s",
			strings.Join(graphNodeIDs(g), "\n  "))
	}
	if !sameKinds(got, want) {
		t.Errorf("pipeline stages from listener to endpoints =\n  %v\nwant\n  %v", got, want)
	}

	// The two HTTP filters must run in the order Envoy reported them, and the
	// route config must hang off the last one -- the router is what performs
	// route selection, so linking it anywhere else would misdescribe the flow.
	chain := graph.NodeID("listener/ingress_http/fc/0")
	hcm := chain + "/nf/0"
	if n := mustGraphNode(t, g, hcm); n.Label != "HTTP connection manager" {
		t.Errorf("network filter label = %q, want the decoded HCM", n.Label)
	}
	if n := mustGraphNode(t, g, hcm+"/hf/0"); n.Label != "envoy.filters.http.cors" {
		t.Errorf("first HTTP filter = %q, want cors", n.Label)
	}
	router := mustGraphNode(t, g, hcm+"/hf/1")
	if router.Label != "envoy.filters.http.router" {
		t.Errorf("second HTTP filter = %q, want the router", router.Label)
	}
	routeCfg := hcm + "/inline-route/local_route"
	if edgeBetween(g, router.ID, routeCfg) == nil {
		t.Errorf("no edge from the terminal HTTP filter to the route config")
	}

	// EDS keys endpoints by service_name, so the edge has to survive the
	// cluster name and the endpoint resource name being different.
	eds := edgeBetween(g, "cluster/service_backend", "endpoints/backend_eds")
	if eds == nil {
		t.Fatal("cluster is not linked to the endpoint set its service_name names")
	}
	if eds.Kind != graph.EdgeRef || eds.Label != "eds" {
		t.Errorf("cluster->endpoints edge = %s/%q, want a ref labelled eds", eds.Kind, eds.Label)
	}
	if n := mustGraphNode(t, g, "endpoints/backend_eds"); n.Sublabel != "2/2 healthy" {
		t.Errorf("endpoints sublabel = %q; EDS with no health checking reports "+
			"UNKNOWN, which counts as serving", n.Sublabel)
	}

	// Nothing may reach back into the listener: the canvas lays the pipeline
	// out left to right and a cycle would have no stable layout.
	if in := edgesInto(g, "listener/ingress_http"); len(in) != 0 {
		t.Errorf("listener has %d inbound edges, want none: %+v", len(in), in)
	}
}

// ------------------------------------------------- 2. a dangling reference

// Routes resolve their cluster per request, so a route naming a cluster that
// does not exist is config Envoy serves happily -- it 503s at request time
// instead. The graph is where that becomes visible, which only works if the
// dump really does carry the route and really does not carry the cluster.
const graphDanglingBootstrap = `{
  "static_resources": {
    "listeners": [{
      "name": "ingress_http",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "filters": [{
          "name": "envoy.filters.network.http_connection_manager",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
            "stat_prefix": "ingress_http",
            "http_filters": [{
              "name": "envoy.filters.http.router",
              "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
            }],
            "rds": {
              "route_config_name": "local_route",
              "config_source": {
                "path_config_source": {"path": "/etc/envoy/rds.json"},
                "resource_api_version": "V3"
              }
            }
          }
        }]
      }]
    }],
    "clusters": [{
      "name": "service_backend",
      "type": "STATIC",
      "connect_timeout": "1s",
      "load_assignment": {
        "cluster_name": "service_backend",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8080}
        }}}]}]
      }
    }]
  }
}`

// RDS rather than an inline route config: an inline route config in the
// bootstrap defaults validate_clusters to true, so Envoy would refuse to start
// and the scenario could not exist. Over RDS the default is false, which is
// exactly why this misconfiguration survives in production long enough to need
// a tool to find it.
const graphDanglingRoutes = `{
  "@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
  "name": "local_route",
  "virtual_hosts": [{
    "name": "backend",
    "domains": ["*"],
    "routes": [
      {"match": {"prefix": "/healthz"}, "route": {"cluster": "service_backend"}},
      {"match": {"prefix": "/"}, "route": {"cluster": "vanished_backend"}}
    ]
  }]
}`

// TestGraphDanglingClusterReference pins down the headline case: the route
// table names an upstream nobody delivered. The graph must keep the name as a
// node rather than dropping the edge, because a route that appears to go
// nowhere and a route that goes somewhere missing look identical once the link
// is gone.
func TestGraphDanglingClusterReference(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "graph-dangling-cluster",
		Bootstrap: graphDanglingBootstrap,
		Files:     map[string]string{"rds.json": discoveryResponse("1", graphDanglingRoutes)},
	})

	ix := e.WaitFor("the RDS route table", func(ix *xds.Index) bool {
		return ix.Get(xds.KindRoute, "local_route") != nil
	})

	// Envoy accepting this is half the point; if a future release started
	// NACKing it, the graph would never see the dangling name at all.
	if rc := ix.Get(xds.KindRoute, "local_route"); rc.ErrorState != nil {
		t.Fatalf("Envoy rejected a route naming an undelivered cluster: %+v", rc.ErrorState)
	}
	if c := ix.Get(xds.KindCluster, "vanished_backend"); c != nil {
		t.Fatalf("the cluster the test means to be missing is in the dump: %+v", c)
	}

	g := graph.Build(ix, graph.Options{})
	checkGraphIntegrity(t, g)

	// An RDS-delivered table is a resource of its own, so it gets the shared
	// "route/<name>" node the UI can link to the raw resource -- unlike an
	// inline route config, which is part of its listener and is keyed under it.
	routeCfg := mustGraphNode(t, g, "route/local_route")
	if routeCfg.Resource == nil || routeCfg.Resource.Name != "local_route" {
		t.Errorf("RDS route config node has resource %+v, want a link back to the "+
			"route resource in the dump", routeCfg.Resource)
	}
	rds := edgeBetween(g, "listener/ingress_http/fc/0/nf/0/hf/0", routeCfg.ID)
	if rds == nil {
		t.Fatal("no edge from the router filter to the RDS route config")
	}
	if rds.Label != "rds" {
		t.Errorf("edge to the route config is labelled %q, want rds", rds.Label)
	}

	missing := graph.NodeID("unresolved/cluster/vanished_backend")
	n := mustGraphNode(t, g, missing)
	if n.Kind != graph.NodeUnresolved {
		t.Errorf("node kind = %s, want %s", n.Kind, graph.NodeUnresolved)
	}
	if n.Status != graph.StatusError {
		t.Errorf("node status = %s, want %s", n.Status, graph.StatusError)
	}

	// The edge matters as much as the node: it is what tells a reader which
	// route is broken, out of however many the table holds.
	in := edgesInto(g, missing)
	if len(in) != 1 {
		t.Fatalf("edges into the unresolved cluster = %d, want exactly one from the route", len(in))
	}
	if from := graphNodeKind(g, in[0].From); from != graph.NodeRoute {
		t.Errorf("unresolved cluster is reached from a %s node, want a %s", from, graph.NodeRoute)
	}
	if in[0].Kind != graph.EdgeRef || in[0].Status != graph.StatusError {
		t.Errorf("edge to the unresolved cluster = %s/%s, want ref/error", in[0].Kind, in[0].Status)
	}
	if !reachableFrom(g, "listener/ingress_http")[missing] {
		t.Error("the unresolved cluster is not reachable from the listener, so the " +
			"canvas would render it detached from the route that names it")
	}

	var found *graph.Problem
	for i, p := range g.Problems {
		if p.NodeID == missing {
			found = &g.Problems[i]
		}
	}
	if found == nil {
		t.Fatalf("no problem reported for the missing cluster; problems = %+v", g.Problems)
	}
	if !strings.Contains(found.Message, "vanished_backend") {
		t.Errorf("problem message %q does not name the missing cluster", found.Message)
	}
	if found.Status != graph.StatusError {
		t.Errorf("problem status = %s, want %s", found.Status, graph.StatusError)
	}

	// The sibling route in the same virtual host still resolves, so the failure
	// is scoped to the one reference rather than poisoning the route table.
	if len(edgesInto(g, "cluster/service_backend")) == 0 {
		t.Error("the route that names a real cluster lost its edge")
	}
}

// ----------------------------------- 3-6. several listeners in one dump

// Three listeners sharing one dump, because the interesting builder behaviour
// only appears when there is more than one root: which nodes belong to which
// listener, whether a cluster two listeners use is one node or two, and whether
// scoping to a root really prunes everything else.
//
// shared_backend is reached from both HTTP listeners; tcp_backend only from the
// TCP one. Ports differ because Envoy will not bind two listeners to the same
// address.
const graphMultiListenerBootstrap = `{
  "static_resources": {
    "listeners": [
      {
        "name": "ingress_http_a",
        "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
        "filter_chains": [{
          "filters": [{
            "name": "envoy.filters.network.http_connection_manager",
            "typed_config": {
              "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
              "stat_prefix": "http_a",
              "http_filters": [{
                "name": "envoy.filters.http.router",
                "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
              }],
              "route_config": {
                "name": "route_a",
                "virtual_hosts": [{
                  "name": "a",
                  "domains": ["a.example.com"],
                  "routes": [{"match": {"prefix": "/"}, "route": {"cluster": "shared_backend"}}]
                }]
              }
            }
          }]
        }]
      },
      {
        "name": "ingress_http_b",
        "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10001}},
        "filter_chains": [{
          "filters": [{
            "name": "envoy.filters.network.http_connection_manager",
            "typed_config": {
              "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
              "stat_prefix": "http_b",
              "http_filters": [{
                "name": "envoy.filters.http.router",
                "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
              }],
              "route_config": {
                "name": "route_b",
                "virtual_hosts": [{
                  "name": "b",
                  "domains": ["b.example.com"],
                  "routes": [{"match": {"prefix": "/"}, "route": {"cluster": "shared_backend"}}]
                }]
              }
            }
          }]
        }]
      },
      {
        "name": "ingress_tcp",
        "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10002}},
        "filter_chains": [{
          "filters": [{
            "name": "envoy.filters.network.tcp_proxy",
            "typed_config": {
              "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
              "stat_prefix": "tcp",
              "cluster": "tcp_backend"
            }
          }]
        }]
      }
    ],
    "clusters": [
      {
        "name": "shared_backend",
        "type": "STATIC",
        "connect_timeout": "1s",
        "load_assignment": {
          "cluster_name": "shared_backend",
          "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
            "socket_address": {"address": "127.0.0.1", "port_value": 8080}
          }}}]}]
        }
      },
      {
        "name": "tcp_backend",
        "type": "STATIC",
        "connect_timeout": "1s",
        "load_assignment": {
          "cluster_name": "tcp_backend",
          "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
            "socket_address": {"address": "127.0.0.1", "port_value": 9090}
          }}}]}]
        }
      }
    ]
  }
}`

// TestGraphMultipleListeners runs its subtests against one container: starting
// Envoy costs seconds and every question below is about the same dump.
func TestGraphMultipleListeners(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "graph-multi-listener",
		Bootstrap: graphMultiListenerBootstrap,
	})
	ix := e.Index()

	all := graph.Build(ix, graph.Options{})
	checkGraphIntegrity(t, all)
	if len(all.Problems) != 0 {
		t.Errorf("a config Envoy accepted produced problems: %+v", all.Problems)
	}

	// Roots are what the UI offers as entry points, so every listener Envoy is
	// serving has to appear and nothing else may.
	t.Run("roots are the listeners", func(t *testing.T) {
		var got []string
		for _, id := range all.Roots {
			got = append(got, string(id))
			if k := graphNodeKind(all, id); k != graph.NodeListener {
				t.Errorf("root %q is a %s, not a listener", id, k)
			}
		}
		sort.Strings(got)
		want := []string{"listener/ingress_http_a", "listener/ingress_http_b", "listener/ingress_tcp"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("roots = %v, want %v", got, want)
		}
	})

	// Scoping to one root is how the UI keeps a real config readable, and it is
	// only useful if the result contains nothing from the other listeners --
	// an orphan node from a listener the reader did not ask about is worse than
	// no filtering at all, because it looks connected to something.
	t.Run("scoped to one listener", func(t *testing.T) {
		g := graph.Build(ix, graph.Options{Roots: []string{"ingress_http_a"}})
		checkGraphIntegrity(t, g)

		if len(g.Roots) != 1 || g.Roots[0] != "listener/ingress_http_a" {
			t.Fatalf("roots = %v, want only the requested listener", g.Roots)
		}
		reached := reachableFrom(g, "listener/ingress_http_a")
		for _, n := range g.Nodes {
			if !reached[n.ID] {
				t.Errorf("node %q is in the scoped graph but unreachable from the root", n.ID)
			}
		}
		for _, id := range []graph.NodeID{
			"listener/ingress_http_b", "listener/ingress_tcp", "cluster/tcp_backend",
		} {
			for _, n := range g.Nodes {
				if n.ID == id {
					t.Errorf("node %q from another listener leaked into the scoped graph", id)
				}
			}
		}
		// The shared cluster is genuinely reachable from this root, so scoping
		// must keep it rather than prune everything not named after the root.
		mustGraphNode(t, g, "cluster/shared_backend")
	})

	// A tcp_proxy hands the connection straight to a cluster with no routing in
	// between, so its pipeline is short by design -- and must not pick up HTTP
	// stages from the listeners sharing the dump.
	t.Run("tcp pipeline", func(t *testing.T) {
		want := []graph.NodeKind{
			graph.NodeListener,
			graph.NodeFilterChain,
			graph.NodeNetworkFilter,
			graph.NodeCluster,
			graph.NodeEndpoints,
		}
		got := pathKinds(all, "listener/ingress_tcp", "cluster/tcp_backend/endpoints")
		if got == nil {
			t.Fatalf("no path from the TCP listener to its endpoints; graph holds:\n  %s",
				strings.Join(graphNodeIDs(all), "\n  "))
		}
		if !sameKinds(got, want) {
			t.Errorf("TCP pipeline stages =\n  %v\nwant\n  %v", got, want)
		}
		if n := mustGraphNode(t, all, "listener/ingress_tcp/fc/0/nf/0"); n.Label != "TCP proxy" {
			t.Errorf("network filter label = %q, want the decoded TcpProxy", n.Label)
		}

		// The two pipelines share a dump, not traffic: a stray edge between
		// them would tell a reader that TCP connections can reach an HTTP
		// backend, which is the sort of wrong answer that gets acted on.
		tcpSide := reachableFrom(all, "listener/ingress_tcp")
		for id := range tcpSide {
			switch graphNodeKind(all, id) {
			case graph.NodeHTTPFilter, graph.NodeRouteConfig, graph.NodeVirtualHost, graph.NodeRoute:
				t.Errorf("node %q is an HTTP stage reachable from the TCP listener", id)
			}
		}
		if tcpSide["cluster/shared_backend"] {
			t.Error("the TCP listener reaches the HTTP listeners' cluster")
		}
		if reachableFrom(all, "listener/ingress_http_a")["cluster/tcp_backend"] {
			t.Error("an HTTP listener reaches the TCP listener's cluster")
		}
	})

	// Two listeners naming the same cluster must converge on one node. A
	// duplicate would be plainly visible in the UI as two copies of the same
	// upstream, and would break the "who sends traffic here" reading of a
	// cluster's inbound edges.
	t.Run("shared cluster is one node", func(t *testing.T) {
		var clusters []graph.NodeID
		for _, n := range all.Nodes {
			if n.Kind == graph.NodeCluster && n.Label == "shared_backend" {
				clusters = append(clusters, n.ID)
			}
		}
		if len(clusters) != 1 {
			t.Fatalf("shared_backend appears as %d nodes (%v), want one", len(clusters), clusters)
		}

		froms := map[graph.NodeID]bool{}
		for _, edge := range edgesInto(all, clusters[0]) {
			froms[edge.From] = true
		}
		if len(froms) != 2 {
			t.Errorf("shared cluster has %d distinct inbound edges (%v), want one per route", len(froms), froms)
		}
		// One from each listener, so the node really is the join point rather
		// than one listener's route linked twice.
		for _, prefix := range []string{"listener/ingress_http_a", "listener/ingress_http_b"} {
			found := false
			for from := range froms {
				if strings.HasPrefix(string(from), prefix) {
					found = true
				}
			}
			if !found {
				t.Errorf("no inbound edge to the shared cluster from %s", prefix)
			}
		}

		// Its endpoints are shared too, for the same reason.
		var eps int
		for _, n := range all.Nodes {
			if n.Kind == graph.NodeEndpoints && strings.Contains(string(n.ID), "shared_backend") {
				eps++
			}
		}
		if eps != 1 {
			t.Errorf("shared cluster has %d endpoint nodes, want one", eps)
		}
	})

	// The whole-dump graph is the widest one this scenario produces, so it is
	// the best place to re-check the invariants with orphan clusters folded in
	// as well: those nodes are added outside the listener walk and so are the
	// likeliest source of a node that no edge can reach.
	t.Run("ids and edges with orphans included", func(t *testing.T) {
		g := graph.Build(ix, graph.Options{IncludeOrphanClusters: true})
		checkGraphIntegrity(t, g)
		if len(g.Nodes) != len(all.Nodes) {
			t.Errorf("including orphans changed the node count (%d -> %d) although "+
				"every cluster in this dump is routed to", len(all.Nodes), len(g.Nodes))
		}
	})
}
