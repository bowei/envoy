package envoytest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Extension types are the one part of a config dump whose shape envoy-view
// cannot predict: which Any bodies resolve depends on which Envoy build
// produced the dump and which go-control-plane packages this binary happens to
// link. internal/xds/resolver.go answers that with an empty placeholder message
// for anything unresolvable, because protojson otherwise fails the whole
// resource over one unknown filter.
//
// Until now that behaviour was pinned only by a hand-written fixture, which is
// a test of what we believe Envoy emits. These scenarios feed the same code a
// real Envoy's dump of configs built from extensions this binary was never
// compiled against.
//
// Note when reading the assertions: "linked" here means linked into the *test*
// binary. internal/envoytest also imports the router filter, the file access
// logger, and upstream HttpProtocolOptions so it can validate bootstraps, so
// the registry here is a superset of the shipped binary's. The types picked
// below -- Lua, Echo, the stream access loggers, raw_buffer, and the rate
// limit filters -- are absent from both.
const (
	luaTypeURL       = "type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua"
	echoTypeURL      = "type.googleapis.com/envoy.extensions.filters.network.echo.v3.Echo"
	corsTypeURL      = "type.googleapis.com/envoy.extensions.filters.http.cors.v3.Cors"
	h2mTypeURL       = "type.googleapis.com/envoy.extensions.filters.http.header_to_metadata.v3.Config"
	httpRLTypeURL    = "type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit"
	netRLTypeURL     = "type.googleapis.com/envoy.extensions.filters.network.local_ratelimit.v3.LocalRateLimit"
	stdoutLogTypeURL = "type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog"
	rawBufferTypeURL = "type.googleapis.com/envoy.extensions.transport_sockets.raw_buffer.v3.RawBuffer"

	hcmTypeURL      = "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager"
	tcpProxyTypeURL = "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy"
	routerTypeURL   = "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"
)

// Markers are strings that exist nowhere except inside an unresolvable
// extension's body, so a test can ask "did this survive into the raw JSON, and
// did it stay out of the typed message" without guessing at field names.
const (
	luaMarker = "envoy-view-lua-marker"
	// The trailing escape stays a backslash-n: this is spliced into a JSON
	// string literal, not into Go source.
	accessLogFmt = `envoy-view-accesslog-marker %RESPONSE_CODE%\n`
)

// unknownHTTPFilter puts a Lua filter -- a core Envoy filter whose Go proto is
// not linked here -- ahead of the router in an otherwise ordinary pipeline.
const unknownHTTPFilter = `{
  "static_resources": {
    "listeners": [{
      "name": "ingress_http",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "filters": [{
          "name": "envoy.filters.network.http_connection_manager",
          "typed_config": {
            "@type": "` + hcmTypeURL + `",
            "stat_prefix": "ingress_http",
            "http_filters": [
              {
                "name": "envoy.filters.http.lua",
                "typed_config": {
                  "@type": "` + luaTypeURL + `",
                  "default_source_code": {
                    "inline_string": "function envoy_on_request(handle)\n  handle:headers():add('x-envoy-view', '` + luaMarker + `')\nend\n"
                  }
                }
              },
              {
                "name": "envoy.filters.http.router",
                "typed_config": {"@type": "` + routerTypeURL + `"}
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
    }],
    "clusters": [` + staticBackendCluster + `]
  }
}`

const staticBackendCluster = `{
      "name": "service_backend",
      "type": "STATIC",
      "connect_timeout": "1s",
      "load_assignment": {
        "cluster_name": "service_backend",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8080}
        }}}]}]
      }
    }`

// TestExtensionUnknownHTTPFilterKeepsListenerUsable is the regression test the
// fallback resolver exists for, run against a real dump instead of a fixture:
// one HTTP filter this binary cannot resolve must cost that filter's body and
// nothing else.
func TestExtensionUnknownHTTPFilterKeepsListenerUsable(t *testing.T) {
	requireUnlinked(t, luaTypeURL)

	e := envoytest.Start(t, envoytest.Config{
		Name:                   "unknown-http-filter",
		Bootstrap:              unknownHTTPFilter,
		AllowUnknownExtensions: true,
	})

	ix := e.Index()
	l := ix.Get(xds.KindListener, "ingress_http")
	if l == nil {
		t.Fatalf("listener ingress_http missing from dump\n%s", ix.Raw)
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener did not decode: %v", l.DecodeErr)
	}
	if l.Message == nil {
		t.Fatal("listener decoded to a nil message")
	}
	if w := ix.Warnings(); len(w) > 0 {
		t.Errorf("an unresolvable filter type produced parse warnings: %v", w)
	}

	// Nothing above is interesting unless this dump really is one a stock
	// decoder chokes on, so ask: protojson refuses the entire listener over the
	// single Any it cannot resolve, which is the whole reason for the fallback.
	requireFallbackNeeded(t, l.Raw)

	// The detail pane renders Raw, so the filter's body must come back byte for
	// byte -- the placeholder is a decoding concession, not a display one.
	got := findTyped(t, l.Raw, luaTypeURL)
	want := map[string]any{
		"@type": luaTypeURL,
		"default_source_code": map[string]any{
			"inline_string": "function envoy_on_request(handle)\n  handle:headers():add('x-envoy-view', '" + luaMarker + "')\nend\n",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Lua config in the dump:\n got %#v\nwant %#v", got, want)
	}

	// And the pipeline still draws: the unknown filter is a stage in the chain,
	// not a hole in it.
	g := e.Graph(graph.Options{})
	const (
		hcmID    = graph.NodeID("listener/ingress_http/fc/0/nf/0")
		luaID    = hcmID + "/hf/0"
		routerID = hcmID + "/hf/1"
		routeID  = hcmID + "/inline-route/local_route"
	)
	for _, pair := range [][2]graph.NodeID{
		{"listener/ingress_http", "listener/ingress_http/fc/0"},
		{"listener/ingress_http/fc/0", hcmID},
		{hcmID, luaID},
		{luaID, routerID},
		{routerID, routeID},
	} {
		if !hasEdge(g, pair[0], pair[1]) {
			t.Errorf("no edge %s -> %s; the pipeline breaks at the unknown filter\n%s",
				pair[0], pair[1], graphDump(g))
		}
	}
	if n := node(g, luaID); n == nil {
		t.Fatalf("no node for the Lua filter\n%s", graphDump(g))
	} else if n.Label != "envoy.filters.http.lua" {
		t.Errorf("Lua filter node label = %q, want the filter name from the listener", n.Label)
	}
	if problems := errorProblems(g); len(problems) > 0 {
		t.Errorf("graph reported problems for a config Envoy accepted: %v", problems)
	}
}

// TestExtensionUnknownFilterBodyIsDroppedFromTypedMessage makes the cost of the
// fallback explicit, because it is easy to assume nothing is lost.
//
// A placeholder has no fields, so the filter's configuration is gone from the
// typed message entirely: the graph builder can see that a Lua filter is in the
// chain and what type it is, but not one byte of what the Lua does. Everything
// the UI shows about the body comes from Resource.Raw. Any future traversal
// that wants to follow a reference out of an extension config -- an RBAC
// principal, a rate limit service cluster -- has to link that extension in
// first; reading it off Message will silently find nothing.
func TestExtensionUnknownFilterBodyIsDroppedFromTypedMessage(t *testing.T) {
	requireUnlinked(t, luaTypeURL)

	e := envoytest.Start(t, envoytest.Config{
		Name:                   "unknown-filter-placeholder",
		Bootstrap:              unknownHTTPFilter,
		AllowUnknownExtensions: true,
	})

	l := e.Index().Get(xds.KindListener, "ingress_http")
	if l == nil {
		t.Fatal("listener ingress_http missing from dump")
	}
	hcm := decodeHCM(t, l)

	lua := hcm.GetHttpFilters()[0]
	if lua.GetName() != "envoy.filters.http.lua" {
		t.Fatalf("first http filter = %q, want the Lua filter", lua.GetName())
	}
	cfg := lua.GetTypedConfig()
	if cfg.GetTypeUrl() != luaTypeURL {
		t.Errorf("type_url = %q, want %q; the type URL is all the UI has to name "+
			"an extension it cannot decode", cfg.GetTypeUrl(), luaTypeURL)
	}
	if n := len(cfg.GetValue()); n != 0 {
		t.Errorf("placeholder carried %d bytes of body, want 0; either the type "+
			"became linked or the resolver changed", n)
	}

	// The whole serialized listener is searched rather than just the filter:
	// this is the assertion that the body is really gone, not relocated.
	wire, err := proto.Marshal(l.Message)
	if err != nil {
		t.Fatalf("marshal decoded listener: %v", err)
	}
	if bytes.Contains(wire, []byte(luaMarker)) {
		t.Error("the Lua source survived into the typed message; the placeholder " +
			"no longer drops extension bodies and this test's premise is stale")
	}
	if !bytes.Contains(l.Raw, []byte(luaMarker)) {
		t.Error("the Lua source is in neither the typed message nor Raw; the UI " +
			"has nothing to show for this filter")
	}

	// The router next to it is a type this binary does link, and the two are
	// indistinguishable by inspection: an empty Router message serializes to
	// zero bytes, exactly like a body the placeholder threw away. Whether an Any
	// was decoded or discarded is only answerable from the registry, so an empty
	// value is evidence of a placeholder only for a config known to be
	// non-empty -- which is why the Lua filter above carries a marker.
	router := hcm.GetHttpFilters()[1].GetTypedConfig()
	if router.GetTypeUrl() != routerTypeURL {
		t.Fatalf("second http filter type = %q, want the router", router.GetTypeUrl())
	}
	if _, err := protoregistry.GlobalTypes.FindMessageByURL(router.GetTypeUrl()); err != nil {
		t.Errorf("the router filter is no longer linked into the test binary (%v), "+
			"so this comparison no longer contrasts anything", err)
	}
}

// unknownNetworkFilter terminates the chain in Echo, whose proto is likewise
// not linked. A network filter is the more dangerous case: it sits directly on
// the Listener message, so a resolver failure there takes out the listener
// rather than a nested config.
const unknownNetworkFilter = `{
  "static_resources": {
    "listeners": [{
      "name": "ingress_echo",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "filters": [{
          "name": "envoy.filters.network.echo",
          "typed_config": {"@type": "` + echoTypeURL + `"}
        }]
      }]
    }]
  }
}`

// TestExtensionUnknownNetworkFilterKeepsListenerUsable covers the same ground
// one level up the tree, where there is no linked-in wrapper type to absorb the
// failure.
func TestExtensionUnknownNetworkFilterKeepsListenerUsable(t *testing.T) {
	requireUnlinked(t, echoTypeURL)

	e := envoytest.Start(t, envoytest.Config{
		Name:                   "unknown-network-filter",
		Bootstrap:              unknownNetworkFilter,
		AllowUnknownExtensions: true,
	})

	ix := e.Index()
	l := ix.Get(xds.KindListener, "ingress_echo")
	if l == nil {
		t.Fatalf("listener ingress_echo missing from dump\n%s", ix.Raw)
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener did not decode: %v", l.DecodeErr)
	}
	if w := ix.Warnings(); len(w) > 0 {
		t.Errorf("an unresolvable network filter produced parse warnings: %v", w)
	}

	requireFallbackNeeded(t, l.Raw)

	got := findTyped(t, l.Raw, echoTypeURL)
	want := map[string]any{"@type": echoTypeURL}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Echo config in the dump:\n got %#v\nwant %#v", got, want)
	}

	// The listener's own fields -- the ones the graph actually reads -- are
	// untouched by the filter it could not resolve.
	msg, ok := l.Message.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("listener message is %T", l.Message)
	}
	if got := msg.GetAddress().GetSocketAddress().GetPortValue(); got != 10000 {
		t.Errorf("listener address port = %d, want 10000", got)
	}
	filters := msg.GetFilterChains()[0].GetFilters()
	if len(filters) != 1 {
		t.Fatalf("filter chain has %d filters, want 1", len(filters))
	}
	if got := filters[0].GetTypedConfig().GetTypeUrl(); got != echoTypeURL {
		t.Errorf("filter type_url = %q, want %q", got, echoTypeURL)
	}

	g := e.Graph(graph.Options{})
	const echoID = graph.NodeID("listener/ingress_echo/fc/0/nf/0")
	n := node(g, echoID)
	if n == nil {
		t.Fatalf("no node for the echo filter\n%s", graphDump(g))
	}
	if n.Status != graph.StatusOK {
		t.Errorf("echo filter node status = %q (%v), want ok: an extension we do "+
			"not traverse is not a broken one", n.Status, n.Notes)
	}
	// Sublabel comes from the Any's type URL and the "type" detail from its full
	// name, which is the whole of what the canvas can say about an extension
	// whose body was dropped.
	if n.Sublabel != "Echo" {
		t.Errorf("echo filter node sublabel = %q, want %q", n.Sublabel, "Echo")
	}
	if !hasEdge(g, "listener/ingress_echo/fc/0", echoID) {
		t.Errorf("filter chain does not link to its only filter\n%s", graphDump(g))
	}
}

// manyUnknownExtensions stacks five distinct unresolvable types into one
// listener: four HTTP filters and a network filter ahead of the HCM.
//
// One unknown type only proves the fallback fires. The placeholder resolver
// caches a synthetic message type per name, and a cache keyed or built wrongly
// would show up here -- as a collision between two placeholders, or as bodies
// leaking from one filter into another.
const manyUnknownExtensions = `{
  "static_resources": {
    "listeners": [{
      "name": "ingress_stacked",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "filters": [
          {
            "name": "envoy.filters.network.local_ratelimit",
            "typed_config": {
              "@type": "` + netRLTypeURL + `",
              "stat_prefix": "tcp_local_rate_limit",
              "token_bucket": {"max_tokens": 100, "tokens_per_fill": 100, "fill_interval": "1s"}
            }
          },
          {
            "name": "envoy.filters.network.http_connection_manager",
            "typed_config": {
              "@type": "` + hcmTypeURL + `",
              "stat_prefix": "ingress_stacked",
              "http_filters": [
                {
                  "name": "envoy.filters.http.header_to_metadata",
                  "typed_config": {
                    "@type": "` + h2mTypeURL + `",
                    "request_rules": [{
                      "header": "x-envoy-view",
                      "on_header_present": {"metadata_namespace": "envoy.lb", "key": "view", "type": "STRING"}
                    }]
                  }
                },
                {
                  "name": "envoy.filters.http.cors",
                  "typed_config": {"@type": "` + corsTypeURL + `"}
                },
                {
                  "name": "envoy.filters.http.local_ratelimit",
                  "typed_config": {
                    "@type": "` + httpRLTypeURL + `",
                    "stat_prefix": "http_local_rate_limit",
                    "token_bucket": {"max_tokens": 100, "tokens_per_fill": 100, "fill_interval": "1s"}
                  }
                },
                {
                  "name": "envoy.filters.http.lua",
                  "typed_config": {
                    "@type": "` + luaTypeURL + `",
                    "default_source_code": {"inline_string": "function envoy_on_request(handle)\nend\n"}
                  }
                },
                {
                  "name": "envoy.filters.http.router",
                  "typed_config": {"@type": "` + routerTypeURL + `"}
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
          }
        ]
      }]
    }],
    "clusters": [` + staticBackendCluster + `]
  }
}`

// TestUnknownExtensionTypesCoexistInOneListener proves the placeholder cache
// handles several distinct unresolvable names in a single unmarshal.
func TestUnknownExtensionTypesCoexistInOneListener(t *testing.T) {
	unknown := []string{netRLTypeURL, h2mTypeURL, corsTypeURL, httpRLTypeURL, luaTypeURL}
	for _, u := range unknown {
		requireUnlinked(t, u)
	}

	e := envoytest.Start(t, envoytest.Config{
		Name:                   "many-unknown-extensions",
		Bootstrap:              manyUnknownExtensions,
		AllowUnknownExtensions: true,
	})

	ix := e.Index()
	l := ix.Get(xds.KindListener, "ingress_stacked")
	if l == nil {
		t.Fatalf("listener ingress_stacked missing from dump\n%s", ix.Raw)
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener did not decode: %v", l.DecodeErr)
	}
	if w := ix.Warnings(); len(w) > 0 {
		t.Errorf("five unresolvable types produced parse warnings: %v", w)
	}

	// Every one of them survives in the raw JSON with its own body.
	for _, u := range unknown {
		if findTyped(t, l.Raw, u) == nil {
			t.Errorf("%s is missing from the raw listener JSON", u)
		}
	}
	if got := findTyped(t, l.Raw, corsTypeURL); len(got) != 1 {
		t.Errorf("the empty Cors config came back as %#v, want only its @type", got)
	}

	msg, ok := l.Message.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("listener message is %T", l.Message)
	}
	filters := msg.GetFilterChains()[0].GetFilters()
	if len(filters) != 2 {
		t.Fatalf("network filters = %d, want the rate limit filter and the HCM", len(filters))
	}
	if got := filters[0].GetTypedConfig().GetTypeUrl(); got != netRLTypeURL {
		t.Errorf("first network filter type = %q, want %q", got, netRLTypeURL)
	}

	// Each placeholder keeps its own identity: the type URLs stay distinct and
	// in order, and none of them carries another's body. Cors would be empty
	// either way; the two rate limit configs and header_to_metadata are the ones
	// whose emptiness here means their bodies were really dropped.
	hcm := decodeHCM(t, l)
	wantOrder := []string{h2mTypeURL, corsTypeURL, httpRLTypeURL, luaTypeURL, routerTypeURL}
	var gotOrder []string
	for _, hf := range hcm.GetHttpFilters() {
		gotOrder = append(gotOrder, hf.GetTypedConfig().GetTypeUrl())
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("http filter types =\n %v\nwant\n %v", gotOrder, wantOrder)
	}
	for i, hf := range hcm.GetHttpFilters()[:4] {
		if n := len(hf.GetTypedConfig().GetValue()); n != 0 {
			t.Errorf("http filter %d (%s) placeholder carried %d body bytes, want 0",
				i, hf.GetName(), n)
		}
	}

	// The chain is drawn end to end even though only the HCM in the middle of
	// it was decodable.
	g := e.Graph(graph.Options{})
	const hcmID = graph.NodeID("listener/ingress_stacked/fc/0/nf/1")
	chain := []graph.NodeID{
		"listener/ingress_stacked",
		"listener/ingress_stacked/fc/0",
		"listener/ingress_stacked/fc/0/nf/0",
		hcmID,
		hcmID + "/hf/0", hcmID + "/hf/1", hcmID + "/hf/2", hcmID + "/hf/3", hcmID + "/hf/4",
		hcmID + "/inline-route/local_route",
	}
	for i := 0; i+1 < len(chain); i++ {
		if !hasEdge(g, chain[i], chain[i+1]) {
			t.Errorf("no edge %s -> %s\n%s", chain[i], chain[i+1], graphDump(g))
		}
	}
	if node(g, "cluster/service_backend") == nil {
		t.Errorf("the route's cluster was never reached\n%s", graphDump(g))
	}
	if problems := errorProblems(g); len(problems) > 0 {
		t.Errorf("graph reported problems for a config Envoy accepted: %v", problems)
	}
}

// unknownOutsideFilterChain moves the unresolvable types off the filter chain
// entirely: an access logger on the listener and a transport socket plus
// protocol options on the cluster. Those are the extension points a
// filter-chain-shaped test would never reach, and the cluster ones exercise a
// second top-level resource type through the same resolver.
const unknownOutsideFilterChain = `{
  "static_resources": {
    "listeners": [{
      "name": "ingress_tcp",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "access_log": [{
        "name": "envoy.access_loggers.stdout",
        "typed_config": {
          "@type": "` + stdoutLogTypeURL + `",
          "log_format": {"text_format_source": {"inline_string": "` + accessLogFmt + `"}}
        }
      }],
      "filter_chains": [{
        "filters": [{
          "name": "envoy.filters.network.tcp_proxy",
          "typed_config": {
            "@type": "` + tcpProxyTypeURL + `",
            "stat_prefix": "tcp",
            "cluster": "service_backend"
          }
        }]
      }]
    }],
    "clusters": [{
      "name": "service_backend",
      "type": "STATIC",
      "connect_timeout": "1s",
      "transport_socket": {
        "name": "envoy.transport_sockets.raw_buffer",
        "typed_config": {"@type": "` + rawBufferTypeURL + `"}
      },
      "typed_extension_protocol_options": {
        "envoy.extensions.upstreams.http.v3.HttpProtocolOptions": {
          "@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
          "explicit_http_config": {"http_protocol_options": {}}
        }
      },
      "load_assignment": {
        "cluster_name": "service_backend",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8080}
        }}}]}]
      }
    }]
  }
}`

// TestExtensionUnknownAccessLoggerAndClusterExtensions checks the extension
// points away from the filter chain, where an unresolvable Any would take down
// a listener or a cluster wholesale.
func TestExtensionUnknownAccessLoggerAndClusterExtensions(t *testing.T) {
	requireUnlinked(t, stdoutLogTypeURL)
	requireUnlinked(t, rawBufferTypeURL)

	e := envoytest.Start(t, envoytest.Config{
		Name:                   "unknown-outside-filter-chain",
		Bootstrap:              unknownOutsideFilterChain,
		AllowUnknownExtensions: true,
	})

	ix := e.Index()
	if w := ix.Warnings(); len(w) > 0 {
		t.Errorf("unresolvable non-filter extensions produced parse warnings: %v", w)
	}

	l := ix.Get(xds.KindListener, "ingress_tcp")
	if l == nil {
		t.Fatalf("listener ingress_tcp missing from dump\n%s", ix.Raw)
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener did not decode: %v", l.DecodeErr)
	}
	msg, ok := l.Message.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("listener message is %T", l.Message)
	}
	logs := msg.GetAccessLog()
	if len(logs) != 1 {
		t.Fatalf("listener access_log entries = %d, want 1", len(logs))
	}
	if got := logs[0].GetTypedConfig().GetTypeUrl(); got != stdoutLogTypeURL {
		t.Errorf("access logger type_url = %q, want %q", got, stdoutLogTypeURL)
	}
	if n := len(logs[0].GetTypedConfig().GetValue()); n != 0 {
		t.Errorf("access logger placeholder carried %d body bytes, want 0", n)
	}
	if !bytes.Contains(l.Raw, []byte("envoy-view-accesslog-marker")) {
		t.Error("the access log format string is missing from the raw listener JSON")
	}

	c := ix.Get(xds.KindCluster, "service_backend")
	if c == nil {
		t.Fatal("cluster service_backend missing from dump")
	}
	if c.DecodeErr != nil {
		t.Fatalf("cluster did not decode: %v", c.DecodeErr)
	}
	cluster, ok := c.Message.(*clusterv3.Cluster)
	if !ok {
		t.Fatalf("cluster message is %T", c.Message)
	}
	if got := cluster.GetTransportSocket().GetTypedConfig().GetTypeUrl(); got != rawBufferTypeURL {
		t.Errorf("transport socket type_url = %q, want %q", got, rawBufferTypeURL)
	}
	// The cluster's own fields are what the graph reads, and an unresolvable
	// transport socket must not cost them.
	if got := cluster.GetConnectTimeout().AsDuration().String(); got != "1s" {
		t.Errorf("connect_timeout = %q, want 1s", got)
	}
	if got := len(cluster.GetLoadAssignment().GetEndpoints()); got != 1 {
		t.Errorf("load_assignment localities = %d, want 1", got)
	}

	// typed_extension_protocol_options is a map<string, Any>, a shape the
	// resolver only meets here. Whether its body survives depends on the
	// binary -- envoytest links HttpProtocolOptions to validate bootstraps,
	// internal/xds does not -- so only the parts that hold either way are
	// asserted: the key, the type URL, and the raw JSON.
	opts := cluster.GetTypedExtensionProtocolOptions()
	po, ok := opts["envoy.extensions.upstreams.http.v3.HttpProtocolOptions"]
	if !ok {
		t.Fatalf("typed_extension_protocol_options keys = %v, want the HTTP options entry", keysOf(opts))
	}
	if !strings.HasSuffix(po.GetTypeUrl(), "envoy.extensions.upstreams.http.v3.HttpProtocolOptions") {
		t.Errorf("protocol options type_url = %q", po.GetTypeUrl())
	}
	if findTyped(t, c.Raw, "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions") == nil {
		t.Error("protocol options are missing from the raw cluster JSON")
	}

	g := e.Graph(graph.Options{})
	if node(g, "cluster/service_backend") == nil {
		t.Errorf("tcp_proxy did not reach its cluster\n%s", graphDump(g))
	}
	if problems := errorProblems(g); len(problems) > 0 {
		t.Errorf("graph reported problems for a config Envoy accepted: %v", problems)
	}
}

// linkedExtensions uses only types internal/xds deliberately links: the HTTP
// connection manager and tcp_proxy.
const linkedExtensions = `{
  "static_resources": {
    "listeners": [
      {
        "name": "ingress_http",
        "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
        "filter_chains": [{
          "filters": [{
            "name": "envoy.filters.network.http_connection_manager",
            "typed_config": {
              "@type": "` + hcmTypeURL + `",
              "stat_prefix": "ingress_http",
              "codec_type": "HTTP2",
              "http_filters": [{
                "name": "envoy.filters.http.router",
                "typed_config": {"@type": "` + routerTypeURL + `"}
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
      },
      {
        "name": "ingress_tcp",
        "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10001}},
        "filter_chains": [{
          "filters": [{
            "name": "envoy.filters.network.tcp_proxy",
            "typed_config": {
              "@type": "` + tcpProxyTypeURL + `",
              "stat_prefix": "tcp",
              "cluster": "service_backend"
            }
          }]
        }]
      }
    ],
    "clusters": [` + staticBackendCluster + `]
  }
}`

// TestExtensionLinkedTypesDecodeForReal is the other half of the contract, and
// the reason the fallback is safe to have: types envoy-view is compiled against
// must still decode into their real messages rather than quietly becoming
// placeholders.
//
// A placeholder here would be invisible -- no error, no warning, just a graph
// that stops at the filter -- so the assertion is on the typed fields the
// traversal depends on, not on the decode succeeding.
func TestExtensionLinkedTypesDecodeForReal(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "linked-extensions",
		Bootstrap: linkedExtensions,
	})

	ix := e.Index()
	if w := ix.Warnings(); len(w) > 0 {
		t.Errorf("parsing a dump of linked-in types produced warnings: %v", w)
	}

	l := ix.Get(xds.KindListener, "ingress_http")
	if l == nil {
		t.Fatalf("listener ingress_http missing from dump\n%s", ix.Raw)
	}
	hcmAny := l.Message.(*listenerv3.Listener).GetFilterChains()[0].GetFilters()[0].GetTypedConfig()
	if len(hcmAny.GetValue()) == 0 {
		t.Fatal("HttpConnectionManager decoded to an empty placeholder; every " +
			"listener would graph as a dead end")
	}
	hcm := &hcmv3.HttpConnectionManager{}
	if err := anypb.UnmarshalTo(hcmAny, hcm, proto.UnmarshalOptions{}); err != nil {
		t.Fatalf("unmarshal HttpConnectionManager: %v", err)
	}
	if hcm.GetStatPrefix() != "ingress_http" {
		t.Errorf("stat_prefix = %q, want %q", hcm.GetStatPrefix(), "ingress_http")
	}
	if hcm.GetCodecType() != hcmv3.HttpConnectionManager_HTTP2 {
		t.Errorf("codec_type = %v, want HTTP2", hcm.GetCodecType())
	}
	if hcm.GetRouteConfig().GetName() != "local_route" {
		t.Errorf("inline route config name = %q, want local_route", hcm.GetRouteConfig().GetName())
	}

	tl := ix.Get(xds.KindListener, "ingress_tcp")
	if tl == nil {
		t.Fatal("listener ingress_tcp missing from dump")
	}
	tcpAny := tl.Message.(*listenerv3.Listener).GetFilterChains()[0].GetFilters()[0].GetTypedConfig()
	tp := &tcpproxyv3.TcpProxy{}
	if err := anypb.UnmarshalTo(tcpAny, tp, proto.UnmarshalOptions{}); err != nil {
		t.Fatalf("unmarshal TcpProxy: %v", err)
	}
	if tp.GetCluster() != "service_backend" {
		t.Errorf("TcpProxy cluster = %q, want service_backend", tp.GetCluster())
	}

	// The traversal is the real assertion: both of these edges exist only
	// because a typed field inside an extension config was readable. Nothing an
	// unknown extension contributes could produce them.
	g := e.Graph(graph.Options{})
	const (
		hcmID      = graph.NodeID("listener/ingress_http/fc/0/nf/0")
		tcpID      = graph.NodeID("listener/ingress_tcp/fc/0/nf/0")
		clusterID  = graph.NodeID("cluster/service_backend")
		inlineRte  = hcmID + "/inline-route/local_route"
		firstRoute = inlineRte + "/vh/0/r/0"
	)
	if !hasEdge(g, hcmID+"/hf/0", inlineRte) {
		t.Errorf("the HCM's inline route config was not reached\n%s", graphDump(g))
	}
	if !hasEdge(g, firstRoute, clusterID) {
		t.Errorf("the route's cluster reference was not resolved\n%s", graphDump(g))
	}
	if !hasEdge(g, tcpID, clusterID) {
		t.Errorf("TcpProxy's cluster reference was not resolved\n%s", graphDump(g))
	}
	if n := node(g, hcmID); n == nil || n.Label != "HTTP connection manager" {
		t.Errorf("HCM node = %+v, want the decoded label", n)
	}
	if problems := errorProblems(g); len(problems) > 0 {
		t.Errorf("graph reported problems for a healthy config: %v", problems)
	}
}

// ------------------------------------------------------------------- helpers

// requireUnlinked fails when a type the scenario needs to be unresolvable has
// become linked, since the test would then pass without testing anything.
func requireUnlinked(t *testing.T, typeURL string) {
	t.Helper()
	if _, err := protoregistry.GlobalTypes.FindMessageByURL(typeURL); err == nil {
		t.Fatalf("%s is linked into this binary, so this scenario no longer "+
			"exercises the placeholder resolver; pick another extension", typeURL)
	}
}

// requireFallbackNeeded asserts that a resource Envoy emitted is one a stock
// protojson decoder cannot handle. Without this, a scenario whose extension
// quietly became resolvable would keep passing while testing nothing.
func requireFallbackNeeded(t *testing.T, raw []byte) {
	t.Helper()
	stock := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := stock.Unmarshal(raw, &listenerv3.Listener{}); err == nil {
		t.Error("this listener decodes without the fallback resolver, so the " +
			"scenario no longer covers unresolvable extension types")
	}
}

// findTyped returns the Any-encoded object in raw whose "@type" matches
// typeURL, wherever it is nested. Searching by type URL rather than by path
// keeps these assertions independent of whether Envoy emitted snake_case or
// lowerCamelCase field names.
func findTyped(t *testing.T, raw []byte, typeURL string) map[string]any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("resource JSON does not parse: %v", err)
	}
	want := strings.TrimPrefix(typeURL, "type.googleapis.com/")

	var walk func(any) map[string]any
	walk = func(v any) map[string]any {
		switch v := v.(type) {
		case map[string]any:
			if s, ok := v["@type"].(string); ok && strings.TrimPrefix(s, "type.googleapis.com/") == want {
				return v
			}
			for _, child := range v {
				if found := walk(child); found != nil {
					return found
				}
			}
		case []any:
			for _, child := range v {
				if found := walk(child); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return walk(doc)
}

// decodeHCM pulls the HttpConnectionManager out of a listener's first filter
// chain. It is the wrapper the unknown HTTP filters live inside, so a scenario
// asserting on them has to get through it first.
func decodeHCM(t *testing.T, r *xds.Resource) *hcmv3.HttpConnectionManager {
	t.Helper()
	msg, ok := r.Message.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("listener message is %T", r.Message)
	}
	for _, f := range msg.GetFilterChains()[0].GetFilters() {
		cfg := f.GetTypedConfig()
		if cfg.GetTypeUrl() != hcmTypeURL {
			continue
		}
		hcm := &hcmv3.HttpConnectionManager{}
		if err := anypb.UnmarshalTo(cfg, hcm, proto.UnmarshalOptions{}); err != nil {
			t.Fatalf("unmarshal HttpConnectionManager: %v", err)
		}
		return hcm
	}
	t.Fatalf("no HttpConnectionManager in listener %q", r.Name)
	return nil
}

// nodeIDs renders the graph's shape for a failure message, because "no edge
// a -> b" is unactionable without knowing what was built instead.
func graphDump(g *graph.Graph) string {
	var b strings.Builder
	b.WriteString("graph nodes:\n")
	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "  %s\n", n.ID)
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  edge %s -> %s\n", e.From, e.To)
	}
	return b.String()
}

func errorProblems(g *graph.Graph) []string {
	var out []string
	for _, p := range g.Problems {
		if p.Status == graph.StatusError {
			out = append(out, string(p.NodeID)+": "+p.Message)
		}
	}
	return out
}

func keysOf(m map[string]*anypb.Any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
