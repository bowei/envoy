package envoytest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"

	"github.com/boweidu/envoy-view/internal/admin"
	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// EDS is the one xDS flavour whose dump section is not an echo of what the
// control plane sent. Envoy rebuilds every ClusterLoadAssignment from its own
// host sets each time /config_dump is fetched, so what envoy-view reads is
// Envoy's view of the endpoints rather than the EDS response that produced
// them. Most of the surprises pinned down below follow from that one fact.

// edsClusterFields is the body of an EDS cluster, minus its braces.
//
// The same cluster is needed in two places that disagree about "@type": inside
// static_resources it is an unknown field and Envoy rejects the bootstrap,
// while as a CDS resource it is required. Sharing the fields keeps the two
// forms from drifting apart.
//
// Each EDS cluster gets a file of its own because Envoy requires a filesystem
// EDS response to carry exactly the one assignment the cluster subscribed for
// and rejects anything else.
func edsClusterFields(name, serviceName, file string) string {
	svc := ""
	if serviceName != "" {
		svc = fmt.Sprintf(`"service_name": %q, `, serviceName)
	}
	return fmt.Sprintf(`
      "name": %q,
      "type": "EDS",
      "connect_timeout": "1s",
      "eds_cluster_config": {%s"eds_config": {
        "path_config_source": {"path": "/etc/envoy/%s"},
        "resource_api_version": "V3"
      }}`, name, svc, file)
}

// edsCluster renders the bootstrap form of an EDS cluster.
func edsCluster(name, serviceName, file string) string {
	return "{" + edsClusterFields(name, serviceName, file) + "}"
}

// edsClusterResource renders the CDS-resource form of an EDS cluster.
func edsClusterResource(name, serviceName, file string) string {
	return `{"@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",` +
		edsClusterFields(name, serviceName, file) + "}"
}

// staticClusters wraps cluster bodies into a bootstrap. None of these scenarios
// has a listener: a cluster and its endpoints are the whole subject, and the
// graph reaches them through Options.IncludeOrphanClusters.
func staticClusters(clusters ...string) string {
	return `{"static_resources": {"clusters": [` + strings.Join(clusters, ",") + `]}}`
}

// cdsFromFile is a bootstrap that takes its clusters from a file, which is how
// a scenario gets clusters Envoy considers dynamic. The distinction matters
// here more than anywhere else: it decides which half of EndpointsConfigDump
// the endpoints land in.
const cdsFromFile = `{
  "dynamic_resources": {
    "cds_config": {
      "path_config_source": {"path": "/etc/envoy/cds.json"},
      "resource_api_version": "V3"
    }
  }
}`

// lbEndpoint is one endpoint at 127.0.0.1:port, optionally with a delivered
// health status. Nothing ever listens on these ports; endpoints appear in the
// dump whether or not they are reachable, and no scenario here sends traffic.
func lbEndpoint(port int, health string) string {
	addr := fmt.Sprintf(`{"endpoint": {"address": {"socket_address": `+
		`{"address": "127.0.0.1", "port_value": %d}}}`, port)
	if health != "" {
		addr += fmt.Sprintf(`, "health_status": %q`, health)
	}
	return addr + "}"
}

// localityGroup is one LocalityLbEndpoints. locality is a JSON object or "".
func localityGroup(locality string, priority int, endpoints ...string) string {
	out := "{"
	if locality != "" {
		out += `"locality": ` + locality + ", "
	}
	if priority != 0 {
		out += fmt.Sprintf(`"priority": %d, `, priority)
	}
	return out + `"lb_endpoints": [` + strings.Join(endpoints, ",") + `]}`
}

// loadAssignment renders a ClusterLoadAssignment resource for an EDS response.
func loadAssignment(name string, groups ...string) string {
	return fmt.Sprintf(
		`{"@type": "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment",
      "cluster_name": %q, "endpoints": [%s]}`, name, strings.Join(groups, ","))
}

// ------------------------------------------------------------------ the tests

// TestEDSBootstrapClusterEndpointsAreStatic pins down which half of
// EndpointsConfigDump an assignment lands in, which is not decided by how the
// assignment arrived.
//
// These endpoints came over a live EDS subscription, but the cluster that
// subscribed was declared in the bootstrap, so Envoy files them under
// static_endpoint_configs and envoy-view reports them as static. "Static" in
// the endpoints section describes the cluster's provenance, not the
// endpoints'.
func TestEDSBootstrapClusterEndpointsAreStatic(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-bootstrap-cluster",
		Bootstrap: staticClusters(edsCluster("eds_backend", "", "eds.json")),
		Files: map[string]string{
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup("", 0, lbEndpoint(8081, ""), lbEndpoint(8082, "")))),
		},
	})

	ix := e.Index()
	r := ix.Get(xds.KindEndpoint, "eds_backend")
	if r == nil {
		t.Fatalf("no endpoint resource for eds_backend\n%s", ix.Raw)
	}
	if r.State != xds.StateStatic {
		t.Errorf("endpoint state = %q, want %q", r.State, xds.StateStatic)
	}
	if r.DecodeErr != nil {
		t.Errorf("endpoints did not decode: %v", r.DecodeErr)
	}

	// The EDS response was version "eds-1", and Envoy keeps none of it. The
	// endpoints section carries neither version_info nor last_updated on its
	// entries, so the version and age columns the UI shows for every other
	// kind are permanently blank for endpoints.
	if r.VersionInfo != "" {
		t.Errorf("endpoint version_info = %q, want empty: Envoy v%s attaches no "+
			"version to endpoint entries. If it now does, envoy-view can show it",
			r.VersionInfo, envoytest.Version())
	}
	if !r.LastUpdated.IsZero() {
		t.Errorf("endpoint last_updated = %v, want zero: Envoy attaches no "+
			"timestamp to endpoint entries", r.LastUpdated)
	}

	cla := mustLoadAssignment(t, r)
	if got := countEndpoints(cla); got != 2 {
		t.Errorf("endpoints in assignment = %d, want 2", got)
	}
	if len(ix.Warnings()) > 0 {
		t.Errorf("parsing produced warnings: %v", ix.Warnings())
	}
}

// TestEDSDynamicClusterEndpointsAreActive is the other half: when CDS delivered
// the cluster, its endpoints are dynamic, and they are keyed by the EDS
// service_name rather than the cluster name.
//
// The service_name case is the one that matters in practice -- it is how Istio
// names every assignment -- and it is the lookup the graph builder performs, so
// a mismatch here would leave every mesh cluster looking endpoint-less.
func TestEDSDynamicClusterEndpointsAreActive(t *testing.T) {
	const service = "outbound|8080||svc.example"

	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-dynamic-cluster",
		Bootstrap: cdsFromFile,
		Files: map[string]string{
			"cds.json": discoveryResponse("cds-1",
				edsClusterResource("eds_backend", "", "eds.json")+","+
					edsClusterResource("named_backend", service, "eds_named.json")),
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup("", 0, lbEndpoint(8081, "")))),
			"eds_named.json": discoveryResponse("eds-1", loadAssignment(service,
				localityGroup("", 0, lbEndpoint(9090, "")))),
		},
	})

	ix := e.WaitFor("both EDS assignments", func(ix *xds.Index) bool {
		return ix.Get(xds.KindEndpoint, "eds_backend") != nil &&
			ix.Get(xds.KindEndpoint, service) != nil
	})

	r := ix.Get(xds.KindEndpoint, "eds_backend")
	if r.State != xds.StateActive {
		t.Errorf("endpoint state = %q, want %q", r.State, xds.StateActive)
	}
	// The cluster carries the CDS version; its endpoints carry nothing, so the
	// two are never comparable in the UI.
	if cl := ix.Get(xds.KindCluster, "eds_backend"); cl.VersionInfo != "cds-1" {
		t.Errorf("cluster version_info = %q, want %q", cl.VersionInfo, "cds-1")
	}
	if r.VersionInfo != "" {
		t.Errorf("dynamic endpoint version_info = %q, want empty", r.VersionInfo)
	}

	if ix.Get(xds.KindEndpoint, "named_backend") != nil {
		t.Error("endpoints were indexed under the cluster name; a cluster with an " +
			"eds_cluster_config.service_name is keyed by that name alone")
	}

	// The graph has to follow the same indirection to draw the edge at all.
	g := e.Graph(graph.Options{IncludeOrphanClusters: true})
	if n := node(g, graph.NodeID("endpoints/"+service)); n == nil {
		t.Errorf("no endpoints node for service %q; nodes: %s", service, nodeIDs(g))
	}
	if !hasEdge(g, "cluster/named_backend", graph.NodeID("endpoints/"+service)) {
		t.Errorf("cluster named_backend is not linked to its endpoints; nodes: %s", nodeIDs(g))
	}
}

// TestEDSIncludeEDSControlsTheEndpointsSection pins the one admin flag
// envoy-view exposes.
//
// Without include_eds the section is not empty, it is absent altogether --
// unlike the scoped-routes and secrets sections, which Envoy emits as "{}"
// whether or not they hold anything. The graph builder reads that absence as
// "not collected" rather than "no endpoints exist", and this is the test that
// says the absence is real.
func TestEDSIncludeEDSControlsTheEndpointsSection(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-include-flag",
		Bootstrap: staticClusters(edsCluster("eds_backend", "", "eds.json")),
		Files: map[string]string{
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup("", 0, lbEndpoint(8081, "")))),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), admin.DefaultTimeout)
	defer cancel()

	without, err := e.Admin().ConfigDump(ctx, false)
	if err != nil {
		t.Fatalf("fetch config dump without include_eds: %v", err)
	}
	with, err := e.Admin().ConfigDump(ctx, true)
	if err != nil {
		t.Fatalf("fetch config dump with include_eds: %v", err)
	}

	const section = "type.googleapis.com/envoy.admin.v3.EndpointsConfigDump"
	if got := dumpSections(t, without); slices.Contains(got, section) {
		t.Errorf("EndpointsConfigDump present without include_eds; sections: %v", got)
	}
	if got := dumpSections(t, with); !slices.Contains(got, section) {
		t.Errorf("EndpointsConfigDump missing with include_eds; sections: %v", got)
	}

	bare := mustParse(t, without)
	if n := bare.Counts()[xds.KindEndpoint]; n != 0 {
		t.Errorf("endpoints indexed without include_eds = %d, want 0", n)
	}
	full := mustParse(t, with)
	if n := full.Counts()[xds.KindEndpoint]; n != 1 {
		t.Errorf("endpoints indexed with include_eds = %d, want 1", n)
	}

	// What the flag costs the user: without it the cluster is drawn as a leaf
	// with a note, and the difference between "no endpoints were fetched" and
	// "this cluster has no endpoints" is exactly what the note exists to make.
	opts := graph.Options{IncludeOrphanClusters: true}
	bareGraph := graph.Build(bare, opts)
	if n := node(bareGraph, "cluster/eds_backend"); n == nil {
		t.Fatalf("no cluster node; nodes: %s", nodeIDs(bareGraph))
	} else if !hasNote(n, "include_eds") {
		t.Errorf("cluster node notes = %v, want one mentioning include_eds", n.Notes)
	}
	if n := node(bareGraph, "endpoints/eds_backend"); n != nil {
		t.Error("an endpoints node was built from a dump that carried no endpoints")
	}
	if n := node(graph.Build(full, opts), "endpoints/eds_backend"); n == nil {
		t.Error("no endpoints node with include_eds")
	}
}

// TestEndpointsHealthStatesDriveTheHealthyCount checks the "N/M healthy" count
// the graph puts on an endpoint set, and records that the dump cannot tell the
// whole truth about health.
//
// Envoy rebuilds the assignment from its host sets, and a host's coarse health
// is only healthy, degraded, or unhealthy. DRAINING therefore comes back as
// UNHEALTHY, and an endpoint delivered with no health_status at all comes back
// as HEALTHY rather than UNKNOWN. envoy-view can never show a draining endpoint
// as draining, no matter what the control plane sent.
func TestEndpointsHealthStatesDriveTheHealthyCount(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-health-states",
		Bootstrap: staticClusters(edsCluster("eds_backend", "", "eds.json")),
		Files: map[string]string{
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup("", 0,
					lbEndpoint(8081, "HEALTHY"),
					lbEndpoint(8082, "UNHEALTHY"),
					lbEndpoint(8083, "DRAINING"),
					lbEndpoint(8084, "")))),
		},
	})

	ix := e.Index()
	r := ix.Get(xds.KindEndpoint, "eds_backend")
	if r == nil {
		t.Fatalf("no endpoint resource for eds_backend\n%s", ix.Raw)
	}
	health := healthByPort(mustLoadAssignment(t, r))
	want := map[uint32]corev3.HealthStatus{
		8081: corev3.HealthStatus_HEALTHY,
		8082: corev3.HealthStatus_UNHEALTHY,
		8083: corev3.HealthStatus_UNHEALTHY, // delivered as DRAINING
		8084: corev3.HealthStatus_HEALTHY,   // delivered with no status at all
	}
	for port, wantStatus := range want {
		if got := health[port]; got != wantStatus {
			t.Errorf("endpoint :%d health_status = %v, want %v", port, got, wantStatus)
		}
	}

	g := e.Graph(graph.Options{IncludeOrphanClusters: true})
	n := node(g, "endpoints/eds_backend")
	if n == nil {
		t.Fatalf("no endpoints node; nodes: %s", nodeIDs(g))
	}
	if n.Sublabel != "2/4 healthy" {
		t.Errorf("endpoints sublabel = %q, want %q", n.Sublabel, "2/4 healthy")
	}
	if n.Status != graph.StatusWarning {
		t.Errorf("endpoints status = %q, want %q: some endpoints are not serving",
			n.Status, graph.StatusWarning)
	}
	if !hasNote(n, "2 of 4 endpoints are not serving") {
		t.Errorf("endpoints notes = %v, want one counting the endpoints that are "+
			"not serving", n.Notes)
	}
}

// TestEndpointsLocalityAndPriorityGroups checks that a multi-locality,
// multi-priority assignment survives into the graph as distinct groups.
//
// Two things are easy to get wrong here and both are load-bearing for the UI:
// the locality string is region/zone/sub_zone joined with slashes, and a
// locality that appears at more than one priority is listed once per group
// rather than deduplicated -- so "east/1a/a1, west/1b/b1, east/1a/a1" is the
// correct rendering of a failover tier, not a bug.
func TestEndpointsLocalityAndPriorityGroups(t *testing.T) {
	const (
		east = `{"region": "east", "zone": "1a", "sub_zone": "a1"}`
		west = `{"region": "west", "zone": "1b", "sub_zone": "b1"}`
	)

	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-locality-priority",
		Bootstrap: staticClusters(edsCluster("eds_backend", "", "eds.json")),
		Files: map[string]string{
			// Deliberately out of order: Envoy sorts groups by priority and then
			// by locality when it rebuilds the assignment, so the dump order is
			// its own, not the control plane's.
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup(west, 0, lbEndpoint(8091, "")),
				localityGroup(east, 0, lbEndpoint(8092, ""), lbEndpoint(8093, "")),
				localityGroup(east, 1, lbEndpoint(8094, "")))),
		},
	})

	ix := e.Index()
	r := ix.Get(xds.KindEndpoint, "eds_backend")
	if r == nil {
		t.Fatalf("no endpoint resource for eds_backend\n%s", ix.Raw)
	}
	cla := mustLoadAssignment(t, r)

	type group struct {
		locality string
		priority uint32
		size     int
	}
	var got []group
	for _, lle := range cla.GetEndpoints() {
		l := lle.GetLocality()
		got = append(got, group{
			locality: strings.Join([]string{l.GetRegion(), l.GetZone(), l.GetSubZone()}, "/"),
			priority: lle.GetPriority(),
			size:     len(lle.GetLbEndpoints()),
		})
	}
	want := []group{
		{"east/1a/a1", 0, 2},
		{"west/1b/b1", 0, 1},
		{"east/1a/a1", 1, 1},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("locality groups = %v, want %v: the groups must not be collapsed "+
			"into one, and priority must survive", got, want)
	}

	g := e.Graph(graph.Options{IncludeOrphanClusters: true})
	n := node(g, "endpoints/eds_backend")
	if n == nil {
		t.Fatalf("no endpoints node; nodes: %s", nodeIDs(g))
	}
	if n.Sublabel != "4/4 healthy" {
		t.Errorf("endpoints sublabel = %q, want %q", n.Sublabel, "4/4 healthy")
	}
	if want := "east/1a/a1, west/1b/b1, east/1a/a1"; detail(n, "localities") != want {
		t.Errorf("localities detail = %q, want %q", detail(n, "localities"), want)
	}
	if got, want := detail(n, "endpoints"), "4"; got != want {
		t.Errorf("endpoints detail = %q, want %q", got, want)
	}
}

// TestEndpointsEmptyAssignmentIsFlaggedOnTheNode covers a cluster that has
// nowhere to send traffic, which is the failure these graphs exist to make
// obvious.
//
// Two ways to get there and Envoy erases the difference between them: an EDS
// response carrying an assignment with no endpoints, and an EDS response
// carrying no assignment at all. Both leave the cluster initialized with an
// empty host set, and the dump reports an empty assignment either way -- so
// envoy-view cannot distinguish "the control plane says zero endpoints" from
// "the control plane never answered".
//
// The node is marked red with a note, but the graph's Problems list -- what the
// UI's problems panel reads -- does not mention it. That is asserted rather
// than wished away: if it changes, this test should be revisited along with the
// panel.
func TestEndpointsEmptyAssignmentIsFlaggedOnTheNode(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name: "eds-empty-assignment",
		Bootstrap: staticClusters(
			edsCluster("empty_backend", "", "eds_empty.json"),
			edsCluster("silent_backend", "", "eds_silent.json"),
		),
		Files: map[string]string{
			"eds_empty.json":  discoveryResponse("eds-1", loadAssignment("empty_backend")),
			"eds_silent.json": discoveryResponse("eds-1", ""),
		},
	})

	ix := e.Index()
	g := graph.Build(ix, graph.Options{IncludeOrphanClusters: true})

	for _, name := range []string{"empty_backend", "silent_backend"} {
		r := ix.Get(xds.KindEndpoint, name)
		if r == nil {
			t.Fatalf("no endpoint resource for %s: Envoy emits an assignment for "+
				"every active cluster even when it holds nothing\n%s", name, ix.Raw)
		}
		if got := countEndpoints(mustLoadAssignment(t, r)); got != 0 {
			t.Errorf("%s: endpoints = %d, want 0", name, got)
		}

		id := graph.NodeID("endpoints/" + name)
		n := node(g, id)
		if n == nil {
			t.Fatalf("%s: no endpoints node; nodes: %s", name, nodeIDs(g))
		}
		if n.Status != graph.StatusError {
			t.Errorf("%s: endpoints status = %q, want %q", name, n.Status, graph.StatusError)
		}
		if n.Sublabel != "0/0 healthy" {
			t.Errorf("%s: endpoints sublabel = %q, want %q", name, n.Sublabel, "0/0 healthy")
		}
		if !hasNote(n, "nowhere to send traffic") {
			t.Errorf("%s: endpoints notes = %v, want one saying the cluster has "+
				"nowhere to send traffic", name, n.Notes)
		}
		if p := findProblem(g, id); p != nil {
			t.Errorf("%s: an empty endpoint set now reaches the problems panel "+
				"(%q); the panel and this comment can be updated", name, p.Message)
		}
	}
}

// TestEDSWarmingClusterHasNoEndpointsAndIsAProblem is the case where the
// endpoints section really does omit a cluster.
//
// A cluster whose EDS subscription has never answered stays warming, and
// warming clusters are not in the endpoints dump at all. That is the one shape
// that reaches the graph's unresolved-reference path, so it is the only way a
// cluster with nowhere to send traffic shows up in the problems panel rather
// than only on the node.
//
// The subscription is pointed at a gRPC management server that is not
// listening, with a long initial_fetch_timeout so the cluster does not give up
// and initialize empty part-way through the test. The xDS cluster has to be in
// the bootstrap: Envoy rejects an api_config_source naming a cluster that
// arrived over CDS.
func TestEDSWarmingClusterHasNoEndpointsAndIsAProblem(t *testing.T) {
	const bootstrap = `{
  "static_resources": {
    "clusters": [{
      "name": "dead_xds",
      "type": "STATIC",
      "connect_timeout": "1s",
      "typed_extension_protocol_options": {
        "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
          "@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
          "explicit_http_config": {"http2_protocol_options": {}}
        }
      },
      "load_assignment": {
        "cluster_name": "dead_xds",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 1}
        }}}]}]
      }
    }]
  },
  "dynamic_resources": {
    "cds_config": {
      "path_config_source": {"path": "/etc/envoy/cds.json"},
      "resource_api_version": "V3"
    }
  }
}`

	const stuckCluster = `{
  "@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",
  "name": "stuck_backend",
  "type": "EDS",
  "connect_timeout": "1s",
  "eds_cluster_config": {"eds_config": {
    "resource_api_version": "V3",
    "initial_fetch_timeout": "600s",
    "api_config_source": {
      "api_type": "GRPC",
      "transport_api_version": "V3",
      "grpc_services": [{"envoy_grpc": {"cluster_name": "dead_xds"}}]
    }
  }}
}`

	healthy := edsClusterResource("eds_backend", "", "eds.json")
	e := envoytest.Start(t, envoytest.Config{
		Name:      "eds-warming-cluster",
		Bootstrap: bootstrap,
		Files: map[string]string{
			"cds.json": discoveryResponse("1", healthy),
			"eds.json": discoveryResponse("eds-1", loadAssignment("eds_backend",
				localityGroup("", 0, lbEndpoint(8081, "")))),
		},
	})

	// The stuck cluster is added after startup on purpose: at startup the
	// cluster manager waits for every cluster to initialize, so a cluster that
	// never initializes would keep Envoy from ever reporting itself live.
	e.WaitFor("the healthy cluster", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "eds_backend") != nil
	})
	e.WriteFile("cds.json", discoveryResponse("2", healthy+","+stuckCluster))

	ix := e.WaitFor("the stuck cluster to appear", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "stuck_backend") != nil
	})

	cl := ix.Get(xds.KindCluster, "stuck_backend")
	if cl.State != xds.StateWarming {
		t.Errorf("cluster state = %q, want %q", cl.State, xds.StateWarming)
	}
	if r := ix.Get(xds.KindEndpoint, "stuck_backend"); r != nil {
		t.Errorf("a warming cluster now has an endpoints entry (%d endpoints); "+
			"the graph's unresolved-endpoints path may be unreachable",
			countEndpoints(mustLoadAssignment(t, r)))
	}
	// Endpoints were collected -- the healthy cluster has some -- so the graph
	// reads the gap as a missing assignment rather than as a dump fetched
	// without include_eds.
	if ix.Counts()[xds.KindEndpoint] == 0 {
		t.Fatal("no endpoints at all in the dump; the scenario cannot distinguish " +
			"a missing assignment from an uncollected section")
	}

	g := graph.Build(ix, graph.Options{IncludeOrphanClusters: true})
	id := graph.NodeID("unresolved/endpoint/stuck_backend")
	n := node(g, id)
	if n == nil {
		t.Fatalf("no unresolved endpoints node for the warming cluster; nodes: %s", nodeIDs(g))
	}
	if n.Status != graph.StatusError {
		t.Errorf("unresolved node status = %q, want %q", n.Status, graph.StatusError)
	}
	p := findProblem(g, id)
	if p == nil {
		t.Fatalf("the warming cluster raised no problem; problems: %+v", g.Problems)
	}
	if p.Status != graph.StatusError {
		t.Errorf("problem status = %q, want %q", p.Status, graph.StatusError)
	}
	if !strings.Contains(p.Message, "nowhere to send traffic") {
		t.Errorf("problem message = %q, want it to say the cluster has nowhere to "+
			"send traffic", p.Message)
	}
	if !hasEdge(g, "cluster/stuck_backend", id) {
		t.Errorf("the warming cluster is not linked to its missing endpoints; nodes: %s", nodeIDs(g))
	}
}

// --------------------------------------------------------------- test helpers

func mustParse(t *testing.T, raw []byte) *xds.Index {
	t.Helper()
	ix, err := xds.Parse(raw)
	if err != nil {
		t.Fatalf("parse config dump: %v", err)
	}
	return ix
}

func mustLoadAssignment(t *testing.T, r *xds.Resource) *endpointv3.ClusterLoadAssignment {
	t.Helper()
	cla, ok := r.Message.(*endpointv3.ClusterLoadAssignment)
	if !ok || cla == nil {
		t.Fatalf("endpoint resource %q did not decode: %v\n%s", r.Name, r.DecodeErr, r.Raw)
	}
	return cla
}

func countEndpoints(cla *endpointv3.ClusterLoadAssignment) int {
	n := 0
	for _, lle := range cla.GetEndpoints() {
		n += len(lle.GetLbEndpoints())
	}
	return n
}

// healthByPort keys health status by port so a failure names the endpoint it
// means rather than an index into a list Envoy is free to reorder.
func healthByPort(cla *endpointv3.ClusterLoadAssignment) map[uint32]corev3.HealthStatus {
	out := make(map[uint32]corev3.HealthStatus)
	for _, lle := range cla.GetEndpoints() {
		for _, lb := range lle.GetLbEndpoints() {
			port := lb.GetEndpoint().GetAddress().GetSocketAddress().GetPortValue()
			out[port] = lb.GetHealthStatus()
		}
	}
	return out
}

// dumpSections lists the "@type" of every section in a raw config dump.
func dumpSections(t *testing.T, raw []byte) []string {
	t.Helper()
	var top struct {
		Configs []struct {
			Type string `json:"@type"`
		} `json:"configs"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("decode config dump: %v", err)
	}
	out := make([]string, 0, len(top.Configs))
	for _, c := range top.Configs {
		out = append(out, c.Type)
	}
	return out
}

func findProblem(g *graph.Graph, id graph.NodeID) *graph.Problem {
	for i, p := range g.Problems {
		if p.NodeID == id {
			return &g.Problems[i]
		}
	}
	return nil
}
