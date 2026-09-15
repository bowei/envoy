package envoytest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// The listener lifecycle states -- active, warming, draining -- are the one
// part of a config dump where a single name means several different configs at
// once, and internal/xds picks between them by precedence. These scenarios
// produce each combination from a real Envoy so that rule is checked against
// behaviour rather than against a fixture we wrote ourselves.
//
// Holding a listener in warming_state turns out to need a subscription that
// never answers at all. Every failure mode of a filesystem RDS source unblocks
// the listener instead of blocking it: an absent file is rejected when the
// listener is built (Envoy requires a path_config_source to exist), and a file
// that is present but holds the wrong route config, or none, makes Envoy give
// up on the route table and serve the listener anyway -- it refuses to let a
// bad control plane wedge a proxy. A gRPC source pointing at a cluster with
// nothing behind it never delivers and never fails, and with
// initial_fetch_timeout disabled Envoy waits for it indefinitely, which is
// exactly the stuck-warming listener an operator would open envoy-view to
// diagnose.
const lifecycleBootstrap = `{
  "static_resources": {
    "clusters": [{
      "name": "xds_never",
      "type": "STATIC",
      "connect_timeout": "1s",
      "typed_extension_protocol_options": {
        "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
          "@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
          "explicit_http_config": {"http2_protocol_options": {}}
        }
      },
      "load_assignment": {
        "cluster_name": "xds_never",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 1}
        }}}]}]
      }
    }]
  },
  "dynamic_resources": {
    "lds_config": {
      "path_config_source": {"path": "/etc/envoy/lds.json"},
      "resource_api_version": "V3"
    },
    "cds_config": {
      "path_config_source": {"path": "/etc/envoy/cds.json"},
      "resource_api_version": "V3"
    }
  }
}`

// tcpListenerOn is a listener with no external dependencies, so it goes active
// the moment LDS delivers it. Every scenario starts from one: Start waits for
// /ready, and a proxy whose only listener is warming never gets there.
func tcpListenerOn(name, port string) string {
	return `{
  "@type": "type.googleapis.com/envoy.config.listener.v3.Listener",
  "name": "` + name + `",
  "address": {"socket_address": {"address": "0.0.0.0", "port_value": ` + port + `}},
  "filter_chains": [{
    "filters": [{
      "name": "envoy.filters.network.tcp_proxy",
      "typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
        "stat_prefix": "tcp",
        "cluster": "backend"
      }
    }]
  }]
}`
}

// httpListenerOn builds an HTTP listener whose route table comes from routeSrc.
func httpListenerOn(name, port, routeSrc string) string {
	return `{
  "@type": "type.googleapis.com/envoy.config.listener.v3.Listener",
  "name": "` + name + `",
  "address": {"socket_address": {"address": "0.0.0.0", "port_value": ` + port + `}},
  "filter_chains": [{
    "filters": [{
      "name": "envoy.filters.network.http_connection_manager",
      "typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
        "stat_prefix": "` + name + `",
        "http_filters": [{
          "name": "envoy.filters.http.router",
          "typed_config": {"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}
        }],
        ` + routeSrc + `
      }
    }]
  }]
}`
}

// inlineRoutes carries the route table in the listener itself, so the listener
// needs nothing else to start serving.
const inlineRoutes = `"route_config": {
          "name": "local_route",
          "virtual_hosts": [{
            "name": "all",
            "domains": ["*"],
            "routes": [{"match": {"prefix": "/"}, "route": {"cluster": "backend"}}]
          }]
        }`

// undeliverableRoutes subscribes to a control plane that will never accept a
// connection, and disables the initial fetch timeout so Envoy keeps waiting.
const undeliverableRoutes = `"rds": {
          "route_config_name": "pending_route",
          "config_source": {
            "resource_api_version": "V3",
            "initial_fetch_timeout": "0s",
            "api_config_source": {
              "api_type": "GRPC",
              "transport_api_version": "V3",
              "grpc_services": [{"envoy_grpc": {"cluster_name": "xds_never"}}]
            }
          }
        }`

// fileRoutes subscribes to the same route config name over the filesystem,
// where the scenario can actually deliver it.
const fileRoutes = `"rds": {
          "route_config_name": "pending_route",
          "config_source": {
            "resource_api_version": "V3",
            "path_config_source": {"path": "/etc/envoy/rds.json"}
          }
        }`

const pendingRouteConfig = `{
  "@type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
  "name": "pending_route",
  "virtual_hosts": [{
    "name": "all",
    "domains": ["*"],
    "routes": [{"match": {"prefix": "/"}, "route": {"cluster": "backend"}}]
  }]
}`

// lifecycleConfig seeds a scenario with one healthy listener; the interesting
// states are pushed in afterwards with WriteFile.
func lifecycleConfig(name string, files map[string]string) envoytest.Config {
	f := map[string]string{
		"lds.json": discoveryResponse("1", tcpListenerOn("ingress_tcp", "10000")),
		"cds.json": discoveryResponse("1", cluster("backend")),
	}
	for k, v := range files {
		f[k] = v
	}
	return envoytest.Config{Name: name, Bootstrap: lifecycleBootstrap, Files: f}
}

// TestLifecycleWarmingListenerIsReportedAndGraphed covers a listener that has
// been delivered but cannot serve: its route table is subscribed from a control
// plane that never answers.
//
// The name exists in exactly one lifecycle section, so there is no precedence
// to apply and Get returns the warming copy. That is worth pinning down
// explicitly: Get is not "the resource that is serving traffic", it is "the
// most authoritative copy of this name", and for a stuck listener nothing is
// serving at all.
func TestLifecycleWarmingListenerIsReportedAndGraphed(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-warming", nil))

	e.WaitFor("the seed listener", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})

	e.WriteFile("lds.json", discoveryResponse("2",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", undeliverableRoutes)))

	ix := e.WaitFor("the new listener to appear", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_http") != nil
	})

	l := ix.Get(xds.KindListener, "ingress_http")
	if l.State != xds.StateWarming {
		t.Fatalf("state = %q, want %q\n%s", l.State, xds.StateWarming, listenerStates(ix))
	}
	if l.VersionInfo != "2" {
		t.Errorf("version_info = %q, want %q", l.VersionInfo, "2")
	}
	if got := ix.OtherStates(xds.KindListener, "ingress_http"); len(got) != 0 {
		t.Errorf("other states = %v, want none: this name exists only as warming", got)
	}
	if ix.Get(xds.KindRoute, "pending_route") != nil {
		t.Error("pending_route is in the dump; the scenario is not testing what it claims")
	}

	// What the operator sees. The listener node itself is not flagged -- the
	// builder only raises a warning when a name appears in more than one state
	// -- so the sole distress signal for a listener that has never served a
	// request is the route it is waiting for showing up as unresolved.
	g := e.Graph(graph.Options{Roots: []string{"ingress_http"}})
	n := listenerNode(t, g, "ingress_http")
	if got := detail(n, "state"); got != string(xds.StateWarming) {
		t.Errorf("node state detail = %q, want %q", got, xds.StateWarming)
	}
	if !hasProblem(g, "pending_route") {
		t.Errorf("no problem mentions the undelivered route table; problems = %v", g.Problems)
	}
}

// TestLifecycleActiveOutranksWarmingUpdate is the precedence rule's real
// justification, and the operational trap it protects against: while a new
// version of a listener warms, the config that is actually serving is the
// previous one, and a tool that reported the newest copy would tell the
// operator the change is live when it is not.
func TestLifecycleActiveOutranksWarmingUpdate(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-active-warming", nil))

	e.WriteFile("lds.json", discoveryResponse("2",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", inlineRoutes)))
	e.WaitFor("the HTTP listener to serve", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_http")
		return l != nil && l.State == xds.StateActive
	})

	// The pending version moves the listener to another port as well as
	// swapping its route source. Changing anything outside the filter chains
	// stops Envoy from treating the push as an in-place filter chain update,
	// which is what keeps the two copies on distinguishable versions -- see
	// TestLifecycleInPlaceUpdateWarmsUnderTheSupersededVersion.
	e.WriteFile("lds.json", discoveryResponse("3",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10002", undeliverableRoutes)))

	ix := e.WaitFor("the update to start warming", func(ix *xds.Index) bool {
		return len(ix.OtherStates(xds.KindListener, "ingress_http")) > 0
	})

	l := ix.Get(xds.KindListener, "ingress_http")
	if l.State != xds.StateActive {
		t.Fatalf("Get returned the %q copy; the active one is what traffic hits\n%s",
			l.State, listenerStates(ix))
	}
	if l.VersionInfo != "2" {
		t.Errorf("serving version_info = %q, want %q", l.VersionInfo, "2")
	}

	others := ix.OtherStates(xds.KindListener, "ingress_http")
	if len(others) != 1 || others[0] != xds.StateWarming {
		t.Fatalf("other states = %v, want exactly [%s]\n%s", others, xds.StateWarming, listenerStates(ix))
	}

	var warming *xds.Resource
	for _, r := range ix.OfKind(xds.KindListener) {
		if r.Name == "ingress_http" && r.State == xds.StateWarming {
			warming = r
		}
	}
	if warming == nil {
		t.Fatalf("no warming copy in OfKind\n%s", listenerStates(ix))
	}
	if warming.VersionInfo != "3" {
		t.Errorf("warming version_info = %q, want the pushed %q", warming.VersionInfo, "3")
	}

	// The graph must show the serving version, not the pending one, and must
	// say that a pending one exists -- otherwise the version on the node is
	// indistinguishable from a fully applied update.
	g := e.Graph(graph.Options{Roots: []string{"ingress_http"}})
	n := listenerNode(t, g, "ingress_http")
	if got := detail(n, "version"); got != "2" {
		t.Errorf("node version detail = %q, want the serving %q", got, "2")
	}
	if got := detail(n, "state"); got != string(xds.StateActive) {
		t.Errorf("node state detail = %q, want %q", got, xds.StateActive)
	}
	if !hasNote(n, "warming") {
		t.Errorf("no note mentions the pending warming update; notes = %v", n.Notes)
	}
	if n.Status != graph.StatusWarning {
		t.Errorf("node status = %q, want %q for a listener with an update stuck warming",
			n.Status, graph.StatusWarning)
	}
}

// TestLifecycleInPlaceUpdateWarmsUnderTheSupersededVersion records a blind spot
// that only a real Envoy reveals.
//
// When a push changes nothing but a listener's filter chains, Envoy performs an
// in-place filter chain update, and the warming copy it creates inherits the
// version_info of the listener it is replacing. Both copies in the dump then
// claim the same version, so "which version is pending" is unanswerable from
// the dump -- the only hint that anything newer exists is the LDS-level
// version_info on the ListenersConfigDump section, which envoy-view does not
// currently read.
func TestLifecycleInPlaceUpdateWarmsUnderTheSupersededVersion(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-inplace", nil))

	e.WriteFile("lds.json", discoveryResponse("2",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", inlineRoutes)))
	e.WaitFor("the HTTP listener to serve", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_http")
		return l != nil && l.State == xds.StateActive
	})

	// Same address, same everything except the route source: an in-place
	// filter chain update.
	e.WriteFile("lds.json", discoveryResponse("3",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", undeliverableRoutes)))

	ix := e.WaitFor("the in-place update to start warming", func(ix *xds.Index) bool {
		return len(ix.OtherStates(xds.KindListener, "ingress_http")) > 0
	})

	copies := 0
	for _, r := range ix.OfKind(xds.KindListener) {
		if r.Name != "ingress_http" {
			continue
		}
		copies++
		if r.VersionInfo != "2" {
			t.Errorf("%s copy version_info = %q, want the superseded %q that Envoy "+
				"stamps on both copies of an in-place update. If Envoy now reports the "+
				"pending version here, envoy-view can tell a warming update apart from "+
				"the config it will replace",
				r.State, r.VersionInfo, "2")
		}
	}
	if copies != 2 {
		t.Errorf("found %d copies of ingress_http, want the active and warming pair\n%s",
			copies, listenerStates(ix))
	}
	if got := sectionVersion(t, ix); got != "3" {
		t.Errorf("ListenersConfigDump version_info = %q, want %q: this is the only "+
			"place the pending LDS version appears", got, "3")
	}
}

// TestLifecycleRemovedListenerDrains covers the state a config dump keeps
// longest and explains least.
//
// A listener removed by the control plane does not vanish: it drains, for
// --drain-time-s (ten minutes by default), and stays in the dump the whole
// time. Nothing is left in any other state, so Get returns the draining copy
// and envoy-view graphs a listener that the control plane has already deleted.
// The state detail on the node is the only thing that says so.
func TestLifecycleRemovedListenerDrains(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-draining", nil))

	e.WaitFor("the seed listener", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})

	// Removal is expressed by omission: the replacement response simply does
	// not mention ingress_tcp.
	e.WriteFile("lds.json", discoveryResponse("2", tcpListenerOn("other_tcp", "10003")))

	ix := e.WaitFor("the removed listener to start draining", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_tcp")
		return l != nil && l.State == xds.StateDraining
	})

	l := ix.Get(xds.KindListener, "ingress_tcp")
	if l.VersionInfo != "1" {
		t.Errorf("draining version_info = %q, want the version it was removed at (%q)",
			l.VersionInfo, "1")
	}
	if got := ix.OtherStates(xds.KindListener, "ingress_tcp"); len(got) != 0 {
		t.Errorf("other states = %v, want none", got)
	}

	g := e.Graph(graph.Options{})
	n := listenerNode(t, g, "ingress_tcp")
	if got := detail(n, "state"); got != string(xds.StateDraining) {
		t.Errorf("node state detail = %q, want %q", got, xds.StateDraining)
	}
}

// TestLifecycleWarmingResolvesWhenTheRouteArrives closes the loop: the pending
// route table shows up, the listener starts serving, and the warming entry is
// gone from the dump.
//
// The route arrives by repointing the subscription at a filesystem source that
// has it, because the never-answering gRPC source that produced the warming
// state cannot be made to answer. What is being asserted is the transition in
// the dump, and that is the same transition either way.
func TestLifecycleWarmingResolvesWhenTheRouteArrives(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-resolve", map[string]string{
		"rds.json": discoveryResponse("1", pendingRouteConfig),
	}))

	e.WaitFor("the seed listener", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})

	e.WriteFile("lds.json", discoveryResponse("2",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", undeliverableRoutes)))
	e.WaitFor("the listener to be stuck warming", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_http")
		return l != nil && l.State == xds.StateWarming
	})

	e.WriteFile("lds.json", discoveryResponse("3",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", fileRoutes)))

	ix := e.WaitFor("the listener to start serving", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_http")
		return l != nil && l.State == xds.StateActive
	})

	if got := ix.OtherStates(xds.KindListener, "ingress_http"); len(got) != 0 {
		t.Errorf("other states = %v, want none: the warming copy should be gone\n%s",
			got, listenerStates(ix))
	}
	for _, r := range ix.OfKind(xds.KindListener) {
		if r.Name == "ingress_http" && r.State == xds.StateWarming {
			t.Errorf("a warming copy survives the resolution\n%s", listenerStates(ix))
		}
	}

	route := ix.Get(xds.KindRoute, "pending_route")
	if route == nil {
		t.Fatalf("the route table that unblocked the listener is not in the dump\n%s",
			listenerStates(ix))
	}
	if route.State != xds.StateActive {
		t.Errorf("route state = %q, want %q", route.State, xds.StateActive)
	}

	// The listener now resolves all the way through to its cluster, which is
	// the difference the operator is looking for.
	g := e.Graph(graph.Options{Roots: []string{"ingress_http"}})
	if len(g.Problems) != 0 {
		t.Errorf("graph still reports problems after the route arrived: %v", g.Problems)
	}
}

// TestLifecycleEffectiveCollapsesLifecycleStates checks the two accessors the
// rest of envoy-view counts on: a dump where two names occupy four lifecycle
// sections must still read as two listeners, one node each, not four.
//
// It also pins the second half of the precedence rule. Replacing a listener's
// address leaves the old copy draining beside the new active one, and the
// active copy has to win, or the graph would render the config that is on its
// way out.
func TestLifecycleEffectiveCollapsesLifecycleStates(t *testing.T) {
	e := envoytest.Start(t, lifecycleConfig("lifecycle-effective", nil))

	e.WriteFile("lds.json", discoveryResponse("2",
		tcpListenerOn("ingress_tcp", "10000")+","+
			httpListenerOn("ingress_http", "10001", inlineRoutes)))
	e.WaitFor("both listeners to serve", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_http")
		return l != nil && l.State == xds.StateActive
	})

	// One name ends up active over draining, the other active over warming.
	e.WriteFile("lds.json", discoveryResponse("3",
		tcpListenerOn("ingress_tcp", "10010")+","+
			httpListenerOn("ingress_http", "10002", undeliverableRoutes)))

	ix := e.WaitFor("both names to occupy two states", func(ix *xds.Index) bool {
		return len(ix.OtherStates(xds.KindListener, "ingress_tcp")) > 0 &&
			len(ix.OtherStates(xds.KindListener, "ingress_http")) > 0
	})

	if n := len(ix.OfKind(xds.KindListener)); n != 4 {
		t.Fatalf("OfKind returned %d listeners, want the 4 lifecycle entries\n%s",
			n, listenerStates(ix))
	}
	if n := len(ix.Effective(xds.KindListener)); n != 2 {
		t.Errorf("Effective returned %d listeners, want one per name (2)\n%s",
			n, listenerStates(ix))
	}
	if n := ix.Counts()[xds.KindListener]; n != 2 {
		t.Errorf("Counts reported %d listeners, want one per name (2)", n)
	}

	want := map[string]xds.State{
		"ingress_tcp":  xds.StateActive,
		"ingress_http": xds.StateActive,
	}
	for _, r := range ix.Effective(xds.KindListener) {
		if r.State != want[r.Name] {
			t.Errorf("effective %s is the %q copy, want %q", r.Name, r.State, want[r.Name])
		}
	}
	if got := ix.OtherStates(xds.KindListener, "ingress_tcp"); len(got) != 1 || got[0] != xds.StateDraining {
		t.Errorf("ingress_tcp other states = %v, want [%s]", got, xds.StateDraining)
	}
	if got := ix.OtherStates(xds.KindListener, "ingress_http"); len(got) != 1 || got[0] != xds.StateWarming {
		t.Errorf("ingress_http other states = %v, want [%s]", got, xds.StateWarming)
	}

	// One root per name, however many states each name occupies.
	g := e.Graph(graph.Options{})
	if len(g.Roots) != 2 {
		t.Errorf("graph has %d roots, want one per listener name (2)", len(g.Roots))
	}
}

// listenerStates renders every lifecycle entry, for failure messages about a
// dump whose interest is precisely which states exist.
func listenerStates(ix *xds.Index) string {
	var b strings.Builder
	b.WriteString("listener states in the dump:\n")
	for _, r := range ix.OfKind(xds.KindListener) {
		b.WriteString("  " + r.Name + " " + string(r.State) + " version=" + r.VersionInfo + "\n")
	}
	return b.String()
}

// sectionVersion returns ListenersConfigDump.version_info, which is the LDS
// subscription's version rather than any one listener's. internal/xds drops it,
// so the test reads it straight out of the raw dump.
func sectionVersion(t *testing.T, ix *xds.Index) string {
	t.Helper()
	var top struct {
		Configs []json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(ix.Raw, &top); err != nil {
		t.Fatalf("decode config dump: %v", err)
	}
	for _, c := range top.Configs {
		var section struct {
			Type        string `json:"@type"`
			VersionInfo string `json:"version_info"`
		}
		if err := json.Unmarshal(c, &section); err != nil {
			continue
		}
		if strings.HasSuffix(section.Type, "ListenersConfigDump") {
			return section.VersionInfo
		}
	}
	t.Fatal("no ListenersConfigDump section in the dump")
	return ""
}

func listenerNode(t *testing.T, g *graph.Graph, name string) *graph.Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.Kind == graph.NodeListener && n.Label == name {
			return n
		}
	}
	t.Fatalf("no listener node %q in the graph", name)
	return nil
}

func hasProblem(g *graph.Graph, substr string) bool {
	for _, p := range g.Problems {
		if strings.Contains(p.Message, substr) {
			return true
		}
	}
	return false
}
