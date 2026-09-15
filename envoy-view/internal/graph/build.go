package graph

import (
	"fmt"
	"strconv"
	"strings"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcmv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/boweidu/envoy-view/internal/xds"
)

// Options controls how much of the dump is expanded into a graph.
type Options struct {
	// Roots limits the graph to these listener names. Empty means every
	// listener, which is only reasonable for small configs -- see the package
	// comment on why the UI picks a root.
	Roots []string

	// IncludeOrphanClusters adds clusters that no route reaches. They are
	// disconnected in the graph, which is the point: a cluster nothing routes
	// to is usually either dead config or a missing route.
	IncludeOrphanClusters bool
}

// Build expands an indexed config dump into a pipeline graph.
func Build(ix *xds.Index, opts Options) *Graph {
	b := &builder{
		ix:         ix,
		g:          &Graph{Nodes: []*Node{}, Edges: []*Edge{}, Roots: []NodeID{}, Problems: []Problem{}},
		nodes:      make(map[NodeID]*Node),
		edges:      make(map[string]bool),
		referenced: make(map[string]bool),
		// Endpoint data is omitted from /config_dump unless include_eds is
		// requested. Without that distinction every EDS cluster would look
		// broken, so an empty endpoints section is read as "not collected"
		// rather than "no endpoints exist".
		edsAvailable: ix.Counts()[xds.KindEndpoint] > 0,
	}

	for _, r := range b.roots(opts.Roots) {
		b.g.Roots = append(b.g.Roots, b.listener(r))
	}

	if opts.IncludeOrphanClusters {
		for _, r := range ix.Effective(xds.KindCluster) {
			if !b.referenced[r.Name] {
				id := b.cluster(r)
				b.addNote(id, "no route in this dump sends traffic here")
				b.raise(id, StatusWarning)
			}
		}
	}

	return b.g
}

type builder struct {
	ix *xds.Index
	g  *Graph

	nodes map[NodeID]*Node
	edges map[string]bool

	// referenced tracks cluster names something routes to, for orphan
	// detection.
	referenced map[string]bool

	edsAvailable bool
}

// roots resolves the requested listener names, defaulting to all of them.
func (b *builder) roots(names []string) []*xds.Resource {
	if len(names) == 0 {
		return b.ix.Effective(xds.KindListener)
	}
	var out []*xds.Resource
	for _, name := range names {
		if r := b.ix.Get(xds.KindListener, name); r != nil {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- listeners

func (b *builder) listener(r *xds.Resource) NodeID {
	id := NodeID("listener/" + r.Name)
	if b.has(id) {
		return id
	}

	n := b.add(&Node{
		ID:       id,
		Kind:     NodeListener,
		Label:    r.Name,
		Status:   StatusOK,
		Resource: &ResourceRef{Kind: xds.KindListener, Name: r.Name},
	})

	msg, ok := r.Message.(*listenerv3.Listener)
	if !ok || msg == nil {
		b.fail(n, fmt.Sprintf("listener config could not be decoded: %v", r.DecodeErr))
		return id
	}

	n.Sublabel = formatAddress(msg.GetAddress())
	n.Details = append(n.Details, Detail{"state", string(r.State)})
	if d := msg.GetTrafficDirection(); d != corev3.TrafficDirection_UNSPECIFIED {
		n.Details = append(n.Details, Detail{"direction", strings.ToLower(d.String())})
	}
	if msg.GetUseOriginalDst().GetValue() {
		n.Details = append(n.Details, Detail{"original dst", "true"})
	}
	b.annotateResource(n, r)

	for i, fc := range msg.GetFilterChains() {
		b.filterChain(id, fc, strconv.Itoa(i), false)
	}
	if fc := msg.GetDefaultFilterChain(); fc != nil {
		b.filterChain(id, fc, "default", true)
	}
	return id
}

func (b *builder) filterChain(listenerID NodeID, fc *listenerv3.FilterChain, key string, isDefault bool) {
	id := NodeID(string(listenerID) + "/fc/" + key)

	label := fc.GetName()
	if label == "" {
		label = "filter chain " + key
	}
	summary, details := filterChainMatchSummary(fc.GetFilterChainMatch())
	if isDefault {
		label = "default filter chain"
		summary = "fallback when no chain matches"
		details = nil
	}

	n := b.add(&Node{
		ID:       id,
		Kind:     NodeFilterChain,
		Label:    label,
		Sublabel: summary,
		Status:   StatusOK,
		Details:  details,
	})
	b.link(listenerID, id, EdgeChain, "", StatusOK)

	if ts := fc.GetTransportSocket(); ts != nil {
		// Not "transport": the filter chain match already uses that label for
		// transport_protocol, which is a different thing.
		n.Details = append(n.Details, Detail{"transport socket", ts.GetName()})
		b.transportSocketSecrets(id, ts)
	}

	if len(fc.GetFilters()) == 0 {
		b.fail(n, "filter chain has no network filters; connections here go nowhere")
		return
	}

	// Network filters run in order, each handing off to the next.
	prev := id
	for i, f := range fc.GetFilters() {
		prev = b.networkFilter(id, prev, f, i)
	}
}

// ------------------------------------------------------------ network filters

// networkFilter adds one network filter and returns the node downstream stages
// should attach to.
func (b *builder) networkFilter(chainID, prev NodeID, f *listenerv3.Filter, i int) NodeID {
	id := NodeID(string(chainID) + "/nf/" + strconv.Itoa(i))
	n := b.add(&Node{
		ID:     id,
		Kind:   NodeNetworkFilter,
		Label:  f.GetName(),
		Status: StatusOK,
	})
	b.link(prev, id, EdgeChain, "", StatusOK)

	cfg := f.GetTypedConfig()
	if cfg == nil {
		if f.GetConfigDiscovery() != nil {
			// ECDS: the filter's config arrives on its own subscription and is
			// not part of this dump.
			n.Sublabel = "config via ECDS"
			b.addNote(id, "filter config is delivered over ECDS and is not in this dump")
			b.raise(id, StatusWarning)
		}
		return id
	}
	n.Sublabel = shortTypeName(cfg)

	switch {
	case isType(cfg, &hcmv3.HttpConnectionManager{}):
		hcm := &hcmv3.HttpConnectionManager{}
		if !decodeAny(cfg, hcm) {
			b.fail(n, "HttpConnectionManager config could not be decoded")
			return id
		}
		b.httpConnectionManager(n, hcm)

	case isType(cfg, &tcpproxyv3.TcpProxy{}):
		tp := &tcpproxyv3.TcpProxy{}
		if !decodeAny(cfg, tp) {
			b.fail(n, "TcpProxy config could not be decoded")
			return id
		}
		b.tcpProxy(n, tp)

	default:
		// Not a filter we traverse. The node still shows its position in the
		// chain and the detail pane still shows its full config.
		n.Details = append(n.Details, Detail{"type", fullTypeName(cfg)})
	}
	return id
}

func (b *builder) httpConnectionManager(n *Node, hcm *hcmv3.HttpConnectionManager) {
	n.Label = "HTTP connection manager"
	n.Sublabel = hcm.GetStatPrefix()
	n.Details = append(n.Details, Detail{"codec", strings.ToLower(hcm.GetCodecType().String())})

	// HTTP filters also run in order, ending at a terminal filter (normally
	// the router) which is what actually performs route selection.
	prev := n.ID
	for i, hf := range hcm.GetHttpFilters() {
		prev = b.httpFilter(n.ID, prev, hf, i)
	}

	switch {
	case hcm.GetRouteConfig() != nil:
		rc := hcm.GetRouteConfig()
		id := NodeID(string(n.ID) + "/inline-route/" + rc.GetName())
		b.routeConfig(id, rc, nil, "inline")
		b.link(prev, id, EdgeRef, "", StatusOK)

	case hcm.GetRds() != nil:
		name := hcm.GetRds().GetRouteConfigName()
		r := b.ix.Get(xds.KindRoute, name)
		if r == nil {
			// The control plane has not delivered this route table, so every
			// request through this listener gets a 404 regardless of what the
			// route config would have said.
			b.link(prev, b.unresolved(xds.KindRoute, name,
				"route configuration %q is referenced via RDS but was never delivered"), EdgeRef, "rds", StatusError)
			return
		}
		rc, ok := r.Message.(*routev3.RouteConfiguration)
		if !ok || rc == nil {
			b.link(prev, b.unresolved(xds.KindRoute, name,
				"route configuration %q could not be decoded"), EdgeRef, "rds", StatusError)
			return
		}
		id := NodeID("route/" + name)
		b.routeConfig(id, rc, r, "rds")
		b.link(prev, id, EdgeRef, "rds", StatusOK)

	case hcm.GetScopedRoutes() != nil:
		b.addNote(n.ID, "routes are selected by scoped RDS, which is not graphed")
		b.raise(n.ID, StatusWarning)

	default:
		b.fail(n, "HttpConnectionManager has no route configuration")
	}
}

func (b *builder) httpFilter(hcmID, prev NodeID, hf *hcmv3.HttpFilter, i int) NodeID {
	id := NodeID(string(hcmID) + "/hf/" + strconv.Itoa(i))
	n := b.add(&Node{
		ID:     id,
		Kind:   NodeHTTPFilter,
		Label:  hf.GetName(),
		Status: StatusOK,
	})
	if cfg := hf.GetTypedConfig(); cfg != nil {
		n.Sublabel = shortTypeName(cfg)
	} else if hf.GetConfigDiscovery() != nil {
		n.Sublabel = "config via ECDS"
	}
	if hf.GetDisabled() {
		n.Details = append(n.Details, Detail{"disabled", "true"})
		b.raise(id, StatusWarning)
	}
	b.link(prev, id, EdgeChain, "", StatusOK)
	return id
}

func (b *builder) tcpProxy(n *Node, tp *tcpproxyv3.TcpProxy) {
	n.Label = "TCP proxy"
	n.Sublabel = tp.GetStatPrefix()

	if c := tp.GetCluster(); c != "" {
		b.clusterRef(n.ID, c, "")
		return
	}
	if wc := tp.GetWeightedClusters(); wc != nil {
		total := uint32(0)
		for _, c := range wc.GetClusters() {
			total += c.GetWeight()
		}
		for _, c := range wc.GetClusters() {
			b.clusterRef(n.ID, c.GetName(), weightLabel(c.GetWeight(), total))
		}
		return
	}
	b.fail(n, "TcpProxy names no upstream cluster")
}

// ------------------------------------------------------------------- routing

func (b *builder) routeConfig(id NodeID, rc *routev3.RouteConfiguration, r *xds.Resource, source string) {
	if b.has(id) {
		return
	}
	n := b.add(&Node{
		ID:       id,
		Kind:     NodeRouteConfig,
		Label:    rc.GetName(),
		Sublabel: plural(len(rc.GetVirtualHosts()), "virtual host", "virtual hosts"),
		Status:   StatusOK,
		Details:  []Detail{{"source", source}},
	})
	if r != nil {
		n.Resource = &ResourceRef{Kind: xds.KindRoute, Name: r.Name}
		b.annotateResource(n, r)
	}
	if len(rc.GetVirtualHosts()) == 0 {
		b.fail(n, "route configuration has no virtual hosts; every request 404s")
		return
	}

	for i, vh := range rc.GetVirtualHosts() {
		b.virtualHost(id, vh, i)
	}
}

func (b *builder) virtualHost(routeID NodeID, vh *routev3.VirtualHost, i int) {
	id := NodeID(string(routeID) + "/vh/" + strconv.Itoa(i))
	domains := vh.GetDomains()
	n := b.add(&Node{
		ID:       id,
		Kind:     NodeVirtualHost,
		Label:    vh.GetName(),
		Sublabel: truncate(strings.Join(domains, ", "), 80),
		Status:   StatusOK,
		Details:  []Detail{{"domains", plural(len(domains), "domain", "domains")}},
	})
	b.link(routeID, id, EdgeChain, "", StatusOK)

	if len(vh.GetRoutes()) == 0 {
		b.fail(n, "virtual host has no routes")
		return
	}
	for j, rt := range vh.GetRoutes() {
		b.route(id, rt, j)
	}
}

func (b *builder) route(vhID NodeID, rt *routev3.Route, i int) {
	id := NodeID(string(vhID) + "/r/" + strconv.Itoa(i))
	label := rt.GetName()
	if label == "" {
		label = "route " + strconv.Itoa(i)
	}
	n := b.add(&Node{
		ID:       id,
		Kind:     NodeRoute,
		Label:    label,
		Sublabel: routeMatchSummary(rt.GetMatch()),
		Status:   StatusOK,
	})
	b.link(vhID, id, EdgeChain, "", StatusOK)
	b.routeAction(n, rt)
}

func (b *builder) routeAction(n *Node, rt *routev3.Route) {
	switch action := rt.GetAction().(type) {
	case *routev3.Route_Route:
		b.forwardAction(n, action.Route)
	case *routev3.Route_Redirect:
		n.Details = append(n.Details, Detail{"action", "redirect"})
	case *routev3.Route_DirectResponse:
		n.Details = append(n.Details, Detail{
			"action", "direct response " + strconv.FormatUint(uint64(action.DirectResponse.GetStatus()), 10),
		})
	case *routev3.Route_FilterAction:
		n.Details = append(n.Details, Detail{"action", "filter action"})
	case *routev3.Route_NonForwardingAction:
		n.Details = append(n.Details, Detail{"action", "non-forwarding"})
	default:
		b.fail(n, "route has no action")
	}
}

func (b *builder) forwardAction(n *Node, ra *routev3.RouteAction) {
	if t := formatDuration(ra.GetTimeout()); t != "" {
		n.Details = append(n.Details, Detail{"timeout", t})
	}
	if pr := ra.GetPrefixRewrite(); pr != "" {
		n.Details = append(n.Details, Detail{"rewrite", pr})
	}

	switch spec := ra.GetClusterSpecifier().(type) {
	case *routev3.RouteAction_Cluster:
		b.clusterRef(n.ID, spec.Cluster, "")

	case *routev3.RouteAction_WeightedClusters:
		// total_weight is deprecated; Envoy sums the per-cluster weights.
		var total uint32
		for _, c := range spec.WeightedClusters.GetClusters() {
			total += c.GetWeight().GetValue()
		}
		for _, c := range spec.WeightedClusters.GetClusters() {
			b.clusterRef(n.ID, c.GetName(), weightLabel(c.GetWeight().GetValue(), total))
		}

	case *routev3.RouteAction_ClusterHeader:
		// The upstream is chosen per-request from a header, so there is no
		// static edge to draw. Say so rather than leaving the route looking
		// like a dead end.
		n.Details = append(n.Details, Detail{"cluster from header", spec.ClusterHeader})
		b.addNote(n.ID, "upstream is selected at request time from header "+spec.ClusterHeader)
		b.raise(n.ID, StatusWarning)

	case *routev3.RouteAction_ClusterSpecifierPlugin:
		n.Details = append(n.Details, Detail{"cluster plugin", spec.ClusterSpecifierPlugin})
		b.addNote(n.ID, "upstream is selected by a cluster specifier plugin")
		b.raise(n.ID, StatusWarning)

	default:
		b.fail(n, "route action names no cluster")
	}
}

func weightLabel(weight, total uint32) string {
	if total == 0 {
		return strconv.FormatUint(uint64(weight), 10)
	}
	pct := float64(weight) * 100 / float64(total)
	return fmt.Sprintf("%d (%.0f%%)", weight, pct)
}

// ------------------------------------------------------------------ clusters

// clusterRef links from to a cluster by name, creating an explicit unresolved
// node when the name is not in the dump.
func (b *builder) clusterRef(from NodeID, name, label string) {
	b.referenced[name] = true

	r := b.ix.Get(xds.KindCluster, name)
	if r == nil {
		to := b.unresolved(xds.KindCluster, name,
			"cluster %q is referenced by a route but is not in this config dump")
		b.link(from, to, EdgeRef, label, StatusError)
		return
	}
	b.link(from, b.cluster(r), EdgeRef, label, StatusOK)
}

func (b *builder) cluster(r *xds.Resource) NodeID {
	id := NodeID("cluster/" + r.Name)
	if b.has(id) {
		return id
	}

	n := b.add(&Node{
		ID:       id,
		Kind:     NodeCluster,
		Label:    r.Name,
		Status:   StatusOK,
		Resource: &ResourceRef{Kind: xds.KindCluster, Name: r.Name},
	})

	msg, ok := r.Message.(*clusterv3.Cluster)
	if !ok || msg == nil {
		b.fail(n, fmt.Sprintf("cluster config could not be decoded: %v", r.DecodeErr))
		return id
	}

	discovery := clusterDiscoveryType(msg)
	n.Sublabel = discovery
	n.Details = append(n.Details,
		Detail{"lb", strings.ToLower(msg.GetLbPolicy().String())},
		Detail{"state", string(r.State)},
	)
	if t := formatDuration(msg.GetConnectTimeout()); t != "" {
		n.Details = append(n.Details, Detail{"connect timeout", t})
	}
	if len(msg.GetHealthChecks()) > 0 {
		n.Details = append(n.Details, Detail{"health checks", strconv.Itoa(len(msg.GetHealthChecks()))})
	}
	b.annotateResource(n, r)

	if ts := msg.GetTransportSocket(); ts != nil {
		b.transportSocketSecrets(id, ts)
	}

	b.clusterEndpoints(id, msg)
	return id
}

// clusterDiscoveryType names how a cluster finds its endpoints. Custom cluster
// types are reported by their extension name, since GetType would misreport
// them as STATIC.
func clusterDiscoveryType(c *clusterv3.Cluster) string {
	if ct := c.GetClusterType(); ct != nil {
		return ct.GetName()
	}
	return c.GetType().String()
}

func (b *builder) clusterEndpoints(clusterID NodeID, msg *clusterv3.Cluster) {
	// A non-EDS cluster carries its endpoints inline.
	if msg.GetClusterType() == nil && msg.GetType() != clusterv3.Cluster_EDS {
		if la := msg.GetLoadAssignment(); la != nil {
			b.endpoints(NodeID(string(clusterID)+"/endpoints"), la, nil)
			b.link(clusterID, NodeID(string(clusterID)+"/endpoints"), EdgeChain, "", StatusOK)
		}
		return
	}
	if msg.GetClusterType() != nil {
		// A custom cluster type resolves hosts its own way; there is nothing
		// in the dump to link to.
		return
	}

	if !b.edsAvailable {
		b.addNote(clusterID, "endpoints were not included in this dump (fetch with include_eds)")
		return
	}

	// EDS keys endpoints by service_name, falling back to the cluster name.
	name := msg.GetEdsClusterConfig().GetServiceName()
	if name == "" {
		name = msg.GetName()
	}

	er := b.ix.Get(xds.KindEndpoint, name)
	if er == nil {
		to := b.unresolved(xds.KindEndpoint, name,
			"no endpoints have been delivered for EDS service %q, so this cluster has nowhere to send traffic")
		b.link(clusterID, to, EdgeRef, "eds", StatusError)
		return
	}
	cla, ok := er.Message.(*endpointv3.ClusterLoadAssignment)
	if !ok || cla == nil {
		to := b.unresolved(xds.KindEndpoint, name, "endpoints for %q could not be decoded")
		b.link(clusterID, to, EdgeRef, "eds", StatusError)
		return
	}
	id := NodeID("endpoints/" + name)
	b.endpoints(id, cla, er)
	b.link(clusterID, id, EdgeRef, "eds", StatusOK)
}

// endpoints adds a collapsed node standing for a whole endpoint set. Endpoints
// outnumber every other resource by orders of magnitude on a real deployment,
// so they are summarised here and expanded on demand by the UI.
func (b *builder) endpoints(id NodeID, cla *endpointv3.ClusterLoadAssignment, r *xds.Resource) {
	if b.has(id) {
		return
	}

	var total, healthy int
	localities := make([]string, 0, len(cla.GetEndpoints()))
	for _, lle := range cla.GetEndpoints() {
		for _, lb := range lle.GetLbEndpoints() {
			total++
			if isServing(lb.GetHealthStatus()) {
				healthy++
			}
		}
		if l := formatLocality(lle.GetLocality()); l != "" {
			localities = append(localities, l)
		}
	}

	n := b.add(&Node{
		ID:        id,
		Kind:      NodeEndpoints,
		Label:     truncate(cla.GetClusterName(), 60),
		Sublabel:  fmt.Sprintf("%d/%d healthy", healthy, total),
		Status:    StatusOK,
		Collapsed: true,
		Details:   []Detail{{"endpoints", strconv.Itoa(total)}},
	})
	if len(localities) > 0 {
		n.Details = append(n.Details, Detail{"localities", truncate(strings.Join(localities, ", "), 60)})
	}
	if r != nil {
		n.Resource = &ResourceRef{Kind: xds.KindEndpoint, Name: r.Name}
		b.annotateResource(n, r)
	}

	switch {
	case total == 0:
		b.addNote(id, "endpoint set is empty; the cluster has nowhere to send traffic")
		b.raise(id, StatusError)
	case healthy == 0:
		b.addNote(id, "no endpoint is currently serving")
		b.raise(id, StatusError)
	case healthy < total:
		b.addNote(id, fmt.Sprintf("%d of %d endpoints are not serving", total-healthy, total))
		b.raise(id, StatusWarning)
	}
}

// isServing reports whether Envoy would load balance to an endpoint in this
// state. UNKNOWN counts: it is what EDS reports when no health checking is
// configured, and those endpoints do receive traffic.
func isServing(s corev3.HealthStatus) bool {
	switch s {
	case corev3.HealthStatus_UNKNOWN, corev3.HealthStatus_HEALTHY, corev3.HealthStatus_DEGRADED:
		return true
	default:
		return false
	}
}

func formatLocality(l *corev3.Locality) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{l.GetRegion(), l.GetZone(), l.GetSubZone()} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

// ------------------------------------------------------------------- secrets

// transportSocketSecrets links a TLS-terminating stage to the SDS secrets it
// depends on. A missing secret is why a listener rejects handshakes, which is
// invisible from the listener config alone.
func (b *builder) transportSocketSecrets(from NodeID, ts *corev3.TransportSocket) {
	cfg := ts.GetTypedConfig()
	if cfg == nil {
		return
	}

	var common *tlsv3.CommonTlsContext
	switch {
	case isType(cfg, &tlsv3.DownstreamTlsContext{}):
		d := &tlsv3.DownstreamTlsContext{}
		if decodeAny(cfg, d) {
			common = d.GetCommonTlsContext()
		}
	case isType(cfg, &tlsv3.UpstreamTlsContext{}):
		u := &tlsv3.UpstreamTlsContext{}
		if decodeAny(cfg, u) {
			common = u.GetCommonTlsContext()
		}
	}
	if common == nil {
		return
	}

	names := make([]string, 0, 2)
	for _, sds := range common.GetTlsCertificateSdsSecretConfigs() {
		names = append(names, sds.GetName())
	}
	if v := common.GetValidationContextSdsSecretConfig(); v != nil {
		names = append(names, v.GetName())
	}
	if c := common.GetCombinedValidationContext(); c != nil {
		if v := c.GetValidationContextSdsSecretConfig(); v != nil {
			names = append(names, v.GetName())
		}
	}

	for _, name := range names {
		if name == "" {
			continue
		}
		r := b.ix.Get(xds.KindSecret, name)
		if r == nil {
			to := b.unresolved(xds.KindSecret, name,
				"secret %q has not been delivered, so TLS here cannot complete a handshake")
			b.link(from, to, EdgeRef, "sds", StatusError)
			continue
		}
		id := NodeID("secret/" + name)
		if !b.has(id) {
			n := b.add(&Node{
				ID:       id,
				Kind:     NodeSecret,
				Label:    name,
				Sublabel: "SDS secret",
				Status:   StatusOK,
				Resource: &ResourceRef{Kind: xds.KindSecret, Name: name},
			})
			b.annotateResource(n, r)
		}
		b.link(from, id, EdgeRef, "sds", StatusOK)
	}
}

// -------------------------------------------------------------------- common

// unresolved adds (or reuses) the placeholder node for a name nothing defines.
// messageFmt takes the name as its single verb.
func (b *builder) unresolved(kind xds.Kind, name, messageFmt string) NodeID {
	id := NodeID("unresolved/" + string(kind) + "/" + name)
	if b.has(id) {
		return id
	}
	b.add(&Node{
		ID:       id,
		Kind:     NodeUnresolved,
		Label:    truncate(name, 60),
		Sublabel: "missing " + string(kind),
		Status:   StatusError,
		Notes:    []string{fmt.Sprintf(messageFmt, name)},
	})
	b.g.Problems = append(b.g.Problems, Problem{
		NodeID:  id,
		Message: fmt.Sprintf(messageFmt, name),
		Status:  StatusError,
	})
	return id
}

// annotateResource carries dump-level resource state onto a node: a rejected
// update means what is graphed is not what the control plane last sent.
func (b *builder) annotateResource(n *Node, r *xds.Resource) {
	if r.VersionInfo != "" {
		n.Details = append(n.Details, Detail{"version", truncate(r.VersionInfo, 32)})
	}
	if es := r.ErrorState; es != nil {
		b.addNote(n.ID, "last update was rejected: "+es.Details)
		b.raise(n.ID, StatusError)
		b.g.Problems = append(b.g.Problems, Problem{
			NodeID:  n.ID,
			Message: fmt.Sprintf("%s %q rejected version %s: %s", r.Kind, r.Name, es.FailedVersionInfo, es.Details),
			Status:  StatusError,
		})
	}
	for _, s := range b.ix.OtherStates(r.Kind, r.Name) {
		b.addNote(n.ID, fmt.Sprintf("a %s copy of this %s also exists", s, r.Kind))
		b.raise(n.ID, StatusWarning)
	}
}

func (b *builder) add(n *Node) *Node {
	if existing, ok := b.nodes[n.ID]; ok {
		return existing
	}
	b.nodes[n.ID] = n
	b.g.Nodes = append(b.g.Nodes, n)
	return n
}

func (b *builder) has(id NodeID) bool {
	_, ok := b.nodes[id]
	return ok
}

func (b *builder) link(from, to NodeID, kind EdgeKind, label string, status Status) {
	id := string(from) + "->" + string(to) + "#" + label
	if b.edges[id] {
		return
	}
	b.edges[id] = true
	b.g.Edges = append(b.g.Edges, &Edge{
		ID: id, From: from, To: to, Kind: kind, Label: label, Status: status,
	})
}

// fail marks a node broken and records why.
func (b *builder) fail(n *Node, msg string) {
	n.Notes = append(n.Notes, msg)
	n.Status = StatusError
	b.g.Problems = append(b.g.Problems, Problem{NodeID: n.ID, Message: msg, Status: StatusError})
}

func (b *builder) addNote(id NodeID, msg string) {
	if n, ok := b.nodes[id]; ok {
		n.Notes = append(n.Notes, msg)
	}
}

// raise escalates a node's status, never lowering it.
func (b *builder) raise(id NodeID, s Status) {
	n, ok := b.nodes[id]
	if !ok {
		return
	}
	rank := map[Status]int{StatusOK: 0, StatusWarning: 1, StatusError: 2}
	if rank[s] > rank[n.Status] {
		n.Status = s
	}
}

// isType reports whether an Any carries the same message type as want.
func isType(a *anypb.Any, want proto.Message) bool {
	return a.MessageIs(want)
}

// decodeAny unmarshals an Any into m. An Any whose body was dropped by the
// placeholder resolver has no value bytes and is treated as undecodable.
func decodeAny(a *anypb.Any, m proto.Message) bool {
	if len(a.GetValue()) == 0 {
		return false
	}
	return a.UnmarshalTo(m) == nil
}
