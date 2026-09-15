package envoytest_test

import (
	"testing"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/xds"
)

// staticHTTP is the smallest configuration that exercises a whole pipeline:
// a listener, an HTTP connection manager with an inline route config, and a
// cluster for the route to point at.
const staticHTTP = `{
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

// TestStartStaticConfig is the framework's own smoke test: if this fails,
// every other scenario failure is noise.
func TestStartStaticConfig(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{Name: "static-http", Bootstrap: staticHTTP})

	ix := e.Index()

	listener := ix.Get(xds.KindListener, "ingress_http")
	if listener == nil {
		t.Fatalf("listener ingress_http missing from dump\n%s", ix.Raw)
	}
	if listener.State != xds.StateStatic {
		t.Errorf("listener state = %q, want %q", listener.State, xds.StateStatic)
	}
	if listener.DecodeErr != nil {
		t.Errorf("listener did not decode: %v", listener.DecodeErr)
	}

	cluster := ix.Get(xds.KindCluster, "service_backend")
	if cluster == nil {
		t.Fatalf("cluster service_backend missing from dump")
	}
	if cluster.State != xds.StateStatic {
		t.Errorf("cluster state = %q, want %q", cluster.State, xds.StateStatic)
	}

	if len(ix.Warnings()) > 0 {
		t.Errorf("parsing a dump from a clean config produced warnings: %v", ix.Warnings())
	}
}
