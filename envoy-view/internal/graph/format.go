package graph

import (
	"fmt"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/boweidu/envoy-view/internal/xds"
)

// ListenerAddress returns a listener resource's bind address, or empty if the
// listener could not be decoded. Exported so callers that only need the address
// -- the UI's root picker, say -- do not have to build a whole graph.
func ListenerAddress(r *xds.Resource) string {
	msg, ok := r.Message.(*listenerv3.Listener)
	if !ok || msg == nil {
		return ""
	}
	return formatAddress(msg.GetAddress())
}

// formatAddress renders an Envoy address the way an operator would write it.
func formatAddress(a *corev3.Address) string {
	switch addr := a.GetAddress().(type) {
	case *corev3.Address_SocketAddress:
		s := addr.SocketAddress
		host := s.GetAddress()
		if strings.Contains(host, ":") {
			host = "[" + host + "]" // IPv6
		}
		if np := s.GetNamedPort(); np != "" {
			return host + ":" + np
		}
		return host + ":" + strconv.FormatUint(uint64(s.GetPortValue()), 10)
	case *corev3.Address_Pipe:
		return "unix:" + addr.Pipe.GetPath()
	case *corev3.Address_EnvoyInternalAddress:
		return "internal:" + addr.EnvoyInternalAddress.GetServerListenerName()
	default:
		return ""
	}
}

// filterChainMatchSummary describes the conditions a connection must meet to
// land on a filter chain. An empty match catches everything.
func filterChainMatchSummary(m *listenerv3.FilterChainMatch) (summary string, details []Detail) {
	if m == nil {
		return "matches any connection", nil
	}
	var parts []string
	add := func(label, value string) {
		if value == "" {
			return
		}
		details = append(details, Detail{Label: label, Value: value})
		parts = append(parts, label+" "+value)
	}

	add("sni", strings.Join(m.GetServerNames(), ", "))
	add("transport", m.GetTransportProtocol())
	add("alpn", strings.Join(m.GetApplicationProtocols(), ", "))
	if p := m.GetDestinationPort(); p != nil {
		add("port", strconv.FormatUint(uint64(p.GetValue()), 10))
	}
	add("dest", cidrList(m.GetPrefixRanges()))
	add("source", cidrList(m.GetSourcePrefixRanges()))
	if ports := m.GetSourcePorts(); len(ports) > 0 {
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.FormatUint(uint64(p), 10)
		}
		add("source port", strings.Join(strs, ", "))
	}
	if st := m.GetSourceType(); st != listenerv3.FilterChainMatch_ANY {
		add("source type", st.String())
	}

	if len(parts) == 0 {
		return "matches any connection", nil
	}
	return strings.Join(parts, " · "), details
}

func cidrList(ranges []*corev3.CidrRange) string {
	if len(ranges) == 0 {
		return ""
	}
	strs := make([]string, len(ranges))
	for i, r := range ranges {
		strs[i] = fmt.Sprintf("%s/%d", r.GetAddressPrefix(), r.GetPrefixLen().GetValue())
	}
	return strings.Join(strs, ", ")
}

// routeMatchSummary renders a route's match condition, e.g. "prefix /v1/".
func routeMatchSummary(m *routev3.RouteMatch) string {
	if m == nil {
		return "(no match)"
	}
	var base string
	switch p := m.GetPathSpecifier().(type) {
	case *routev3.RouteMatch_Prefix:
		base = "prefix " + quoteEmpty(p.Prefix)
	case *routev3.RouteMatch_Path:
		base = "path " + quoteEmpty(p.Path)
	case *routev3.RouteMatch_SafeRegex:
		base = "regex " + quoteEmpty(p.SafeRegex.GetRegex())
	case *routev3.RouteMatch_PathSeparatedPrefix:
		base = "path-prefix " + quoteEmpty(p.PathSeparatedPrefix)
	case *routev3.RouteMatch_ConnectMatcher_:
		base = "CONNECT"
	case *routev3.RouteMatch_PathMatchPolicy:
		base = "path-matcher " + p.PathMatchPolicy.GetName()
	default:
		base = "any path"
	}

	var extra []string
	if n := len(m.GetHeaders()); n > 0 {
		extra = append(extra, plural(n, "header", "headers"))
	}
	if n := len(m.GetQueryParameters()); n > 0 {
		extra = append(extra, plural(n, "query param", "query params"))
	}
	if m.GetRuntimeFraction() != nil {
		extra = append(extra, "runtime fraction")
	}
	if len(extra) > 0 {
		base += " + " + strings.Join(extra, ", ")
	}
	return base
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// shortTypeName is the last dotted component of an Any's type URL, which is the
// part that identifies the extension: "...v3.HttpConnectionManager" -> the name.
func shortTypeName(a *anypb.Any) string {
	url := a.GetTypeUrl()
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		url = url[i+1:]
	}
	if i := strings.LastIndexByte(url, '.'); i >= 0 {
		return url[i+1:]
	}
	return url
}

// fullTypeName is the Any type URL without its type host.
func fullTypeName(a *anypb.Any) string {
	url := a.GetTypeUrl()
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		return url[i+1:]
	}
	return url
}

func formatDuration(d *durationpb.Duration) string {
	if d == nil {
		return ""
	}
	return d.AsDuration().String()
}

// truncate keeps node faces from being stretched by pathological names such as
// Istio's "outbound|8080|v1|svc.ns.svc.cluster.local" style identifiers.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
