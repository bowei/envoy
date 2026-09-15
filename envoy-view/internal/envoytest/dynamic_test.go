package envoytest_test

import (
	"testing"
	"time"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Filesystem xDS: Envoy subscribes to a file holding a DiscoveryResponse and
// re-reads it when the file is replaced.
//
// This is the cheapest way to produce genuinely dynamic resources -- the
// active/warming states, version_info, and NACK error_state that only ever
// appear under a control plane. A gRPC ADS server would be more faithful, but
// it would also add a gRPC dependency to reach states that arrive through the
// same subscription machinery either way.
const fsDynamicBootstrap = `{
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

// discoveryResponse wraps resources the way a filesystem subscription expects.
func discoveryResponse(version, resources string) string {
	return `{"version_info": "` + version + `", "resources": [` + resources + `]}`
}

const tcpListener = `{
  "@type": "type.googleapis.com/envoy.config.listener.v3.Listener",
  "name": "ingress_tcp",
  "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
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

func cluster(name string) string {
	return `{
  "@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",
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

func dynamicConfig(name string) envoytest.Config {
	return envoytest.Config{
		Name:      name,
		Bootstrap: fsDynamicBootstrap,
		Files: map[string]string{
			"lds.json": discoveryResponse("1", tcpListener),
			"cds.json": discoveryResponse("1", cluster("backend")),
		},
	}
}

// TestDynamicResourcesAreActive pins down what a dynamically delivered
// resource looks like in a dump: which section it lands in, and that
// version_info is carried through. The hand-written dynamic_full.json fixture
// asserts this shape, so this test is what makes that fixture trustworthy.
func TestDynamicResourcesAreActive(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("dynamic-active"))

	ix := e.WaitFor("the dynamic listener to go active", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})

	listener := ix.Get(xds.KindListener, "ingress_tcp")
	if listener.State != xds.StateActive {
		t.Errorf("listener state = %q, want %q", listener.State, xds.StateActive)
	}
	if listener.VersionInfo != "1" {
		t.Errorf("listener version_info = %q, want %q", listener.VersionInfo, "1")
	}
	if listener.LastUpdated.IsZero() {
		t.Error("listener last_updated is zero; the UI shows this field")
	}

	cl := ix.Get(xds.KindCluster, "backend")
	if cl == nil {
		t.Fatal("cluster backend missing from dump")
	}
	if cl.State != xds.StateActive {
		t.Errorf("cluster state = %q, want %q", cl.State, xds.StateActive)
	}
}

// TestVersionInfoIsPerResourceNotPerResponse pins down what the version column
// in the UI actually means.
//
// Pushing version "2" carrying an unchanged cluster and a new one does not
// stamp "2" on both. Envoy skips clusters whose config did not change, so the
// unchanged one keeps the version at which it last actually changed. The
// number therefore answers "when did this resource last change", not "how
// current is this Envoy" -- and two resources showing different versions is
// normal, not evidence of a half-applied update.
func TestVersionInfoIsPerResourceNotPerResponse(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("dynamic-update"))

	e.WaitFor("the initial cluster", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "backend") != nil
	})

	e.WriteFile("cds.json", discoveryResponse("2", cluster("backend")+","+cluster("backend_v2")))

	ix := e.WaitFor("the second cluster to arrive", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "backend_v2") != nil
	})

	if got := ix.Get(xds.KindCluster, "backend_v2").VersionInfo; got != "2" {
		t.Errorf("newly added cluster version_info = %q, want %q", got, "2")
	}
	if got := ix.Get(xds.KindCluster, "backend").VersionInfo; got != "1" {
		t.Errorf("unchanged cluster version_info = %q, want the version it last "+
			"changed at (%q)", got, "1")
	}
}

// TestChangedResourceTakesNewVersion is the other half: a resource whose
// config really did change does pick up the new version.
func TestChangedResourceTakesNewVersion(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("dynamic-changed"))

	e.WaitFor("the initial cluster", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "backend") != nil
	})

	changed := `{
      "@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",
      "name": "backend",
      "type": "STATIC",
      "connect_timeout": "7s",
      "load_assignment": {"cluster_name": "backend", "endpoints": [{"lb_endpoints": [
        {"endpoint": {"address": {"socket_address": {"address": "127.0.0.1", "port_value": 8080}}}}
      ]}]}
    }`
	e.WriteFile("cds.json", discoveryResponse("2", changed))

	ix := e.WaitFor("the modified cluster to take version 2", func(ix *xds.Index) bool {
		c := ix.Get(xds.KindCluster, "backend")
		return c != nil && c.VersionInfo == "2"
	})
	if cl := ix.Get(xds.KindCluster, "backend"); cl.State != xds.StateActive {
		t.Errorf("state = %q, want %q", cl.State, xds.StateActive)
	}
}

// duplicateChainListener passes proto validation and fails when Envoy applies
// it, because two filter chains claim the same match. An application failure
// is what produces error_state; a proto validation failure does not.
const duplicateChainListener = `{
  "@type": "type.googleapis.com/envoy.config.listener.v3.Listener",
  "name": "ingress_tcp",
  "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
  "filter_chains": [
    {"filter_chain_match": {}, "filters": [{
      "name": "envoy.filters.network.tcp_proxy",
      "typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
        "stat_prefix": "a", "cluster": "backend"
      }
    }]},
    {"filter_chain_match": {}, "filters": [{
      "name": "envoy.filters.network.tcp_proxy",
      "typed_config": {
        "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
        "stat_prefix": "b", "cluster": "backend"
      }
    }]}
  ]
}`

// TestRejectedListenerHasErrorState is the scenario the tool exists for: a
// control plane pushed something Envoy would not take, so what is serving is
// older than what was sent, and the serving config alone does not say so.
func TestRejectedListenerHasErrorState(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("lds-nack"))

	e.WaitFor("the initial listener", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})

	e.WriteFile("lds.json", discoveryResponse("2-bad", duplicateChainListener))

	ix := e.WaitFor("the rejected update to be recorded", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_tcp")
		return l != nil && l.ErrorState != nil
	})

	l := ix.Get(xds.KindListener, "ingress_tcp")
	if l.VersionInfo != "1" {
		t.Errorf("serving version_info = %q, want the pre-rejection %q", l.VersionInfo, "1")
	}
	if l.State != xds.StateActive {
		t.Errorf("state = %q, want the old config still %q", l.State, xds.StateActive)
	}
	if l.ErrorState.Details == "" {
		t.Error("error_state.details is empty; the UI has nothing to show")
	}
	if l.ErrorState.LastUpdateAttempt.IsZero() {
		t.Error("error_state.last_update_attempt is zero")
	}

	// failed_version_info is deliberately not asserted. A filesystem
	// subscription leaves it empty, so envoy-view must render a rejection
	// without one rather than printing "rejected version <empty>".
}

// TestRejectedListenerAddsNamelessDumpEntry pins down a shape that would
// otherwise be discovered by a crash.
//
// Alongside the named listener, Envoy appends a second dynamic_listeners entry
// that carries only error_state -- no name, no state. Anything iterating the
// list and reading .name gets an empty string, and indexing by it invents a
// listener called "". envoy-view must skip these.
func TestRejectedListenerAddsNamelessDumpEntry(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("lds-nack-nameless"))

	e.WaitFor("the initial listener", func(ix *xds.Index) bool {
		return ix.Get(xds.KindListener, "ingress_tcp") != nil
	})
	e.WriteFile("lds.json", discoveryResponse("2-bad", duplicateChainListener))
	ix := e.WaitFor("the rejection", func(ix *xds.Index) bool {
		l := ix.Get(xds.KindListener, "ingress_tcp")
		return l != nil && l.ErrorState != nil
	})

	if got := ix.Get(xds.KindListener, ""); got != nil {
		t.Errorf("a listener with an empty name was indexed from the nameless "+
			"error entry: %+v", got)
	}
	for _, r := range ix.OfKind(xds.KindListener) {
		if r.Name == "" {
			t.Errorf("listener with empty name in the index: state=%q", r.State)
		}
	}
	if n := len(ix.Effective(xds.KindListener)); n != 1 {
		t.Errorf("effective listeners = %d, want 1", n)
	}
}

// TestClusterRejectionIsInvisibleInTheDump records a real blind spot rather
// than a behaviour we want.
//
// ClustersConfigDump.DynamicCluster has an error_state field, but Envoy does
// not populate it: a cluster update rejected at application time leaves the
// dump looking exactly like a healthy one, with the stale cluster still
// listed. The rejection appears only in the log and in stats.
//
// This test exists so that if a future Envoy starts filling the field, it
// fails and tells us envoy-view can now surface cluster NACKs too.
func TestClusterRejectionIsInvisibleInTheDump(t *testing.T) {
	e := envoytest.Start(t, dynamicConfig("cds-nack"))

	e.WaitFor("the initial cluster", func(ix *xds.Index) bool {
		return ix.Get(xds.KindCluster, "backend") != nil
	})

	// An EDS cluster with no eds_cluster_config passes proto validation and
	// fails when Envoy builds the cluster.
	bad := `{
      "@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",
      "name": "backend",
      "type": "EDS",
      "connect_timeout": "1s"
    }`
	e.WriteFile("cds.json", discoveryResponse("2-bad", bad))

	// Nothing to wait for: the assertion is that nothing changes. Give Envoy
	// long enough to have read and rejected the file.
	time.Sleep(3 * time.Second)

	cl := e.Index().Get(xds.KindCluster, "backend")
	if cl == nil {
		t.Fatal("cluster backend disappeared after a rejected update")
	}
	if cl.VersionInfo != "1" {
		t.Errorf("version_info = %q, want the stale %q", cl.VersionInfo, "1")
	}
	if cl.ErrorState != nil {
		t.Errorf("CDS now reports error_state (%+v) -- envoy-view can surface "+
			"cluster NACKs; update the graph builder and delete this test",
			cl.ErrorState)
	}
}
