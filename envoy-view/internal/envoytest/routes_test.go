package envoytest_test

import (
	"strings"
	"testing"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Routing is the part of a config people open a viewer to understand: "why
// does this request go there" is a question about virtual host domains, match
// order, and what a route's action names. The builder's answer to it was only
// ever checked against fixtures we wrote ourselves, so this file runs the same
// assertions against a route table a real Envoy accepted and echoed back.

// rdsBootstrap serves its route table over RDS from a file rather than inlining
// it in the HCM. That is how it arrives in production, and it is the only way
// the route configuration becomes an xDS resource of its own -- with its own
// version and state -- instead of a field inside the listener.
var rdsBootstrap = `{
  "static_resources": {
    "listeners": [{
      "name": "edge",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "filters": [{
          "name": "envoy.filters.network.http_connection_manager",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
            "stat_prefix": "edge",
            "http_filters": [{
              "name": "envoy.filters.http.router",
              "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
            }],
            "rds": {
              "route_config_name": "edge_routes",
              "config_source": {
                "path_config_source": {"path": "/etc/envoy/rds.json"},
                "resource_api_version": "V3"
              }
            }
          }
        }]
      }]
    }],
    "clusters": [
      ` + staticCluster("api_backend") + `,
      ` + staticCluster("checkout_v1") + `,
      ` + staticCluster("checkout_v2") + `,
      ` + staticCluster("checkout_canary") + `
    ]
  }
}`

func staticCluster(name string) string {
	return `{
      "name": "` + name + `",
      "type": "STATIC",
      "connect_timeout": "1s",
      "load_assignment": {
        "cluster_name": "` + name + `",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8080}
        }}}]}]
      }
    }`
}

// edgeRoutes is deliberately one table rather than several: every route shape
// that renders differently -- each match kind, each action kind, a catch-all
// host -- costs nothing extra once the container is up, and keeping them
// together also checks they do not bleed into each other.
const edgeRoutes = `{
  "@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
  "name": "edge_routes",
  "virtual_hosts": [
    {
      "name": "api",
      "domains": ["api.example.com", "api.internal"],
      "routes": [
        {
          "name": "health",
          "match": {"path": "/healthz"},
          "direct_response": {"status": 200, "body": {"inline_string": "ok"}}
        },
        {
          "name": "versioned-items",
          "match": {"safe_regex": {"regex": "^/v[0-9]+/items(/.*)?$"}},
          "route": {"cluster": "api_backend"}
        },
        {
          "name": "canary-optin",
          "match": {
            "prefix": "/checkout",
            "headers": [{"name": "x-canary", "string_match": {"exact": "true"}}]
          },
          "route": {"cluster": "checkout_canary"}
        },
        {
          "name": "checkout-split",
          "match": {"prefix": "/checkout"},
          "route": {
            "weighted_clusters": {
              "clusters": [
                {"name": "checkout_v1", "weight": 70},
                {"name": "checkout_v2", "weight": 25},
                {"name": "checkout_canary", "weight": 5}
              ]
            }
          }
        },
        {
          "name": "api-default",
          "match": {"prefix": "/"},
          "route": {"cluster": "api_backend"}
        }
      ]
    },
    {
      "name": "legacy",
      "domains": ["legacy.example.com"],
      "routes": [{
        "name": "moved",
        "match": {"prefix": "/"},
        "redirect": {"host_redirect": "api.example.com", "response_code": "MOVED_PERMANENTLY"}
      }]
    },
    {
      "name": "catch_all",
      "domains": ["*"],
      "routes": [{
        "name": "unknown-host",
        "match": {"prefix": "/"},
        "direct_response": {"status": 404, "body": {"inline_string": "no virtual host\n"}}
      }]
    }
  ]
}`

func rdsConfig(name, routes string) envoytest.Config {
	return envoytest.Config{
		Name:      name,
		Bootstrap: rdsBootstrap,
		Files:     map[string]string{"rds.json": discoveryResponse("7", routes)},
	}
}

// TestRouteConfigShapes drives one richly configured Envoy and asserts each
// route shape the UI has to render. Subtests share the container because a
// container start dominates the cost of everything below it.
func TestRouteConfigShapes(t *testing.T) {
	e := envoytest.Start(t, rdsConfig("routes-shapes", edgeRoutes))

	ix := e.WaitFor("the RDS route table to arrive", func(ix *xds.Index) bool {
		return ix.Get(xds.KindRoute, "edge_routes") != nil
	})
	g := graph.Build(ix, graph.Options{})

	// A clean config must produce a clean graph. Every later subtest asserts
	// something more specific, but a stray problem here would mean the builder
	// invents faults in a config Envoy is happily serving.
	if len(g.Problems) > 0 {
		t.Errorf("graph of a valid route table reported problems: %+v", g.Problems)
	}

	t.Run("rds resource is independent of its listener", func(t *testing.T) {
		rc := ix.Get(xds.KindRoute, "edge_routes")
		if rc.State != xds.StateActive {
			t.Errorf("route config state = %q, want %q: an RDS table lands in "+
				"dynamic_route_configs, not alongside the static listener",
				rc.State, xds.StateActive)
		}
		if rc.VersionInfo != "7" {
			t.Errorf("route config version_info = %q, want %q", rc.VersionInfo, "7")
		}

		// The listener that subscribes to it is static and so has no version at
		// all: the two resources age independently, which is exactly why the UI
		// shows a version per resource rather than one for the whole dump.
		l := ix.Get(xds.KindListener, "edge")
		if l == nil {
			t.Fatal("listener edge missing from dump")
		}
		if l.State != xds.StateStatic {
			t.Errorf("listener state = %q, want %q", l.State, xds.StateStatic)
		}
		if l.VersionInfo != "" {
			t.Errorf("static listener version_info = %q, want empty", l.VersionInfo)
		}

		n := findNode(t, g, graph.NodeRouteConfig, "edge_routes")
		if n.Resource == nil || n.Resource.Kind != xds.KindRoute || n.Resource.Name != "edge_routes" {
			t.Errorf("route config node resource ref = %+v, want the route resource", n.Resource)
		}
		if got := detail(n, "source"); got != "rds" {
			t.Errorf("route config source detail = %q, want %q", got, "rds")
		}
		if got := detail(n, "version"); got != "7" {
			t.Errorf("route config version detail = %q, want %q", got, "7")
		}
		if got, want := n.Sublabel, "3 virtual hosts"; got != want {
			t.Errorf("route config sublabel = %q, want %q", got, want)
		}
	})

	t.Run("hcm links to the route config by name", func(t *testing.T) {
		rcNode := findNode(t, g, graph.NodeRouteConfig, "edge_routes")

		var in []*graph.Edge
		for _, e := range g.Edges {
			if e.To == rcNode.ID {
				in = append(in, e)
			}
		}
		if len(in) != 1 {
			t.Fatalf("edges into the route config = %d, want 1: %+v", len(in), in)
		}
		if in[0].Kind != graph.EdgeRef || in[0].Label != "rds" {
			t.Errorf("edge into route config = %+v, want a %q ref labelled %q",
				in[0], graph.EdgeRef, "rds")
		}

		// The link hangs off the router filter rather than the HCM node itself,
		// because the router is the filter that actually performs route
		// selection -- the graph is drawing where the request goes, not which
		// proto field holds the name.
		from := node(g, in[0].From)
		if from == nil {
			t.Fatalf("edge %q comes from a node that is not in the graph", in[0].ID)
		}
		if from.Kind != graph.NodeHTTPFilter || from.Label != "envoy.filters.http.router" {
			t.Errorf("route config is linked from %s %q, want the router HTTP filter",
				from.Kind, from.Label)
		}
	})

	t.Run("virtual hosts keep their domains", func(t *testing.T) {
		hosts := nodesOfKind(g, graph.NodeVirtualHost)
		if got := labels(hosts); !equal(got, []string{"api", "legacy", "catch_all"}) {
			t.Fatalf("virtual host nodes = %v, want one per virtual host in order", got)
		}

		want := map[string]struct {
			sublabel string
			count    string
		}{
			"api":       {"api.example.com, api.internal", "2 domains"},
			"legacy":    {"legacy.example.com", "1 domain"},
			"catch_all": {"*", "1 domain"},
		}
		for _, n := range hosts {
			w := want[n.Label]
			if n.Sublabel != w.sublabel {
				t.Errorf("virtual host %q sublabel = %q, want %q", n.Label, n.Sublabel, w.sublabel)
			}
			if got := detail(n, "domains"); got != w.count {
				t.Errorf("virtual host %q domains detail = %q, want %q", n.Label, got, w.count)
			}
		}

		// Routes must hang off the host that owns them: a route shown under the
		// wrong domain is worse than one not shown at all.
		byHost := map[string][]string{
			"api":       {"health", "versioned-items", "canary-optin", "checkout-split", "api-default"},
			"legacy":    {"moved"},
			"catch_all": {"unknown-host"},
		}
		for host, wantRoutes := range byHost {
			got := children(g, findNode(t, g, graph.NodeVirtualHost, host).ID, graph.NodeRoute)
			if !equal(got, wantRoutes) {
				t.Errorf("routes under virtual host %q = %v, want %v", host, got, wantRoutes)
			}
		}
	})

	t.Run("match summaries distinguish the match kinds", func(t *testing.T) {
		want := map[string]string{
			"health":          "path /healthz",
			"versioned-items": `regex ^/v[0-9]+/items(/.*)?$`,
			"canary-optin":    "prefix /checkout + 1 header",
			"checkout-split":  "prefix /checkout",
			"api-default":     "prefix /",
		}
		seen := make(map[string]string, len(want))
		for label, wantSummary := range want {
			n := findNode(t, g, graph.NodeRoute, label)
			if n.Sublabel != wantSummary {
				t.Errorf("route %q match summary = %q, want %q", label, n.Sublabel, wantSummary)
			}
			if prev, dup := seen[n.Sublabel]; dup {
				t.Errorf("routes %q and %q render the same summary %q; the UI cannot "+
					"tell them apart", prev, label, n.Sublabel)
			}
			seen[n.Sublabel] = label
		}
	})

	t.Run("weighted clusters fan out to every backend", func(t *testing.T) {
		n := findNode(t, g, graph.NodeRoute, "checkout-split")

		got := make(map[string]string)
		for _, e := range edgesFrom(g, n.ID) {
			to := node(g, e.To)
			if to == nil {
				t.Fatalf("edge %q points at a node that is not in the graph", e.ID)
			}
			if to.Kind != graph.NodeCluster {
				t.Errorf("weighted route has a %s edge to %s %q, want clusters only",
					e.Kind, to.Kind, to.Label)
				continue
			}
			got[to.Label] = e.Label
		}

		// All three, not just the first: a split that renders as one backend
		// hides exactly the traffic an operator is trying to account for.
		want := map[string]string{
			"checkout_v1":     "70 (70%)",
			"checkout_v2":     "25 (25%)",
			"checkout_canary": "5 (5%)",
		}
		if len(got) != len(want) {
			t.Fatalf("weighted route reaches %d clusters (%v), want %d", len(got), got, len(want))
		}
		for name, wantLabel := range want {
			if got[name] != wantLabel {
				t.Errorf("edge to %q labelled %q, want %q: the weight is the whole "+
					"point of a split", name, got[name], wantLabel)
			}
		}

		// The weights live only on the edges -- the route node face says nothing
		// about being a split. Pinned so that a renderer which drops edge labels
		// is understood to be dropping the split itself.
		if d := detail(n, "action"); d != "" {
			t.Errorf("weighted route has an action detail %q; the split is "+
				"expressed as edges, and duplicating it would need keeping in sync", d)
		}
	})

	t.Run("redirect and direct response are terminal", func(t *testing.T) {
		// These actions answer the request themselves, so there is no cluster
		// to draw an edge to. The risk is the builder treating "no cluster" as
		// "missing cluster" and inventing an unresolved node plus a problem,
		// which would fill the problems panel with routes that are working
		// exactly as configured.
		cases := []struct {
			route  string
			action string
		}{
			{"moved", "redirect"},
			{"health", "direct response 200"},
			{"unknown-host", "direct response 404"},
		}
		for _, tc := range cases {
			n := findNode(t, g, graph.NodeRoute, tc.route)
			if got := detail(n, "action"); got != tc.action {
				t.Errorf("route %q action detail = %q, want %q", tc.route, got, tc.action)
			}
			if n.Status != graph.StatusOK {
				t.Errorf("route %q status = %q, want %q; a terminal action is not a fault",
					tc.route, n.Status, graph.StatusOK)
			}
			if len(n.Notes) > 0 {
				t.Errorf("route %q carries notes %v; a terminal action needs no explaining",
					tc.route, n.Notes)
			}
			if out := edgesFrom(g, n.ID); len(out) != 0 {
				t.Errorf("route %q has outgoing edges %+v, want none: the pipeline "+
					"ends here", tc.route, out)
			}
			for _, p := range g.Problems {
				if p.NodeID == n.ID {
					t.Errorf("route %q raised problem %q", tc.route, p.Message)
				}
			}
		}

		if n := len(nodesOfKind(g, graph.NodeUnresolved)); n != 0 {
			t.Errorf("graph has %d unresolved nodes; every reference in this table "+
				"resolves", n)
		}
	})
}

// ghostRoutes points at a cluster nothing defines.
//
// It has to arrive over RDS rather than in the bootstrap: validate_clusters
// defaults to true for a static route configuration, so Envoy would refuse to
// start. Delivered dynamically the default flips to false, Envoy serves the
// table, and every request to that route 503s -- the dangling reference that
// envoy-view exists to make obvious.
const ghostRoutes = `{
  "@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
  "name": "edge_routes",
  "virtual_hosts": [{
    "name": "api",
    "domains": ["*"],
    "routes": [
      {"name": "good", "match": {"prefix": "/api"}, "route": {"cluster": "api_backend"}},
      {"name": "ghost", "match": {"prefix": "/"}, "route": {"cluster": "ghost_backend"}}
    ]
  }]
}`

const healthyRoutes = `{
  "@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
  "name": "edge_routes",
  "virtual_hosts": [{
    "name": "api",
    "domains": ["*"],
    "routes": [{"name": "good", "match": {"prefix": "/"}, "route": {"cluster": "api_backend"}}]
  }]
}`

// TestRDSUpdateIntroducesDanglingCluster is the failure mode a config dump
// viewer is bought for: the control plane pushed a route table naming a cluster
// that was never delivered, Envoy accepted it without complaint, and nothing in
// the serving config says so. The dump is consistent; the deployment is broken.
func TestRDSUpdateIntroducesDanglingCluster(t *testing.T) {
	e := envoytest.Start(t, rdsConfig("routes-dangling", healthyRoutes))

	e.WaitFor("the initial route table", func(ix *xds.Index) bool {
		return ix.Get(xds.KindRoute, "edge_routes") != nil
	})
	if n := len(e.Graph(graph.Options{}).Problems); n != 0 {
		t.Fatalf("the healthy starting point already has %d problems", n)
	}

	e.WriteFile("rds.json", discoveryResponse("8", ghostRoutes))

	ix := e.WaitFor("the route table naming a missing cluster", func(ix *xds.Index) bool {
		rc := ix.Get(xds.KindRoute, "edge_routes")
		return rc != nil && rc.VersionInfo == "8"
	})

	// Envoy took it: no NACK, and the table is serving. Anything the operator
	// is going to learn, they learn from the graph.
	rc := ix.Get(xds.KindRoute, "edge_routes")
	if rc.ErrorState != nil {
		t.Errorf("route config was rejected (%+v); RDS is expected to accept a "+
			"table with an undeliverable cluster", rc.ErrorState)
	}
	if rc.State != xds.StateActive {
		t.Errorf("route config state = %q, want %q", rc.State, xds.StateActive)
	}

	g := graph.Build(ix, graph.Options{})

	ghost := findNode(t, g, graph.NodeRoute, "ghost")
	out := edgesFrom(g, ghost.ID)
	if len(out) != 1 {
		t.Fatalf("route ghost has %d outgoing edges, want 1", len(out))
	}
	to := node(g, out[0].To)
	if to == nil || to.Kind != graph.NodeUnresolved {
		t.Fatalf("route ghost points at %+v, want an unresolved node", to)
	}
	if to.Label != "ghost_backend" {
		t.Errorf("unresolved node label = %q, want the cluster name %q", to.Label, "ghost_backend")
	}
	if to.Status != graph.StatusError || out[0].Status != graph.StatusError {
		t.Errorf("unresolved node status = %q and edge status = %q, want both %q",
			to.Status, out[0].Status, graph.StatusError)
	}

	var found bool
	for _, p := range g.Problems {
		if p.NodeID == to.ID {
			found = true
			if !strings.Contains(p.Message, "ghost_backend") {
				t.Errorf("problem message %q does not name the missing cluster", p.Message)
			}
		}
	}
	if !found {
		t.Errorf("no problem raised for the dangling cluster; problems = %+v", g.Problems)
	}

	// The sibling route still resolves: one bad reference must not take the
	// rest of the table down with it.
	good := findNode(t, g, graph.NodeRoute, "good")
	if got := children(g, good.ID, graph.NodeCluster); !equal(got, []string{"api_backend"}) {
		t.Errorf("route good reaches clusters %v, want [api_backend]", got)
	}
}

// ------------------------------------------------------------------- helpers

func nodesOfKind(g *graph.Graph, kind graph.NodeKind) []*graph.Node {
	var out []*graph.Node
	for _, n := range g.Nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func findNode(t *testing.T, g *graph.Graph, kind graph.NodeKind, label string) *graph.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Kind == kind && n.Label == label {
			return n
		}
	}
	t.Fatalf("no %s node labelled %q; graph has %v", kind, label, labels(nodesOfKind(g, kind)))
	return nil
}

func labels(nodes []*graph.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Label
	}
	return out
}

func edgesFrom(g *graph.Graph, id graph.NodeID) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.Edges {
		if e.From == id {
			out = append(out, e)
		}
	}
	return out
}

// children returns the labels of the nodes of one kind directly downstream of
// id, in graph order.
func children(g *graph.Graph, id graph.NodeID, kind graph.NodeKind) []string {
	var out []string
	for _, e := range edgesFrom(g, id) {
		if n := node(g, e.To); n != nil && n.Kind == kind {
			out = append(out, n.Label)
		}
	}
	return out
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
