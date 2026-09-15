package xds

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"
)

// Kind identifies the type of an xDS resource.
type Kind string

const (
	KindListener Kind = "listener" // envoy.config.listener.v3.Listener
	KindCluster  Kind = "cluster"  // envoy.config.cluster.v3.Cluster
	KindRoute    Kind = "route"    // envoy.config.route.v3.RouteConfiguration
	KindEndpoint Kind = "endpoint" // envoy.config.endpoint.v3.ClusterLoadAssignment
	KindSecret   Kind = "secret"   // envoy.extensions.transport_sockets.tls.v3.Secret
)

// Kinds lists every kind in a stable display order.
var Kinds = []Kind{KindListener, KindRoute, KindCluster, KindEndpoint, KindSecret}

// State is where a resource sits in the config dump's lifecycle sections.
type State string

const (
	StateStatic   State = "static"   // from the bootstrap config
	StateActive   State = "active"   // dynamically delivered and serving
	StateWarming  State = "warming"  // delivered, not yet serving
	StateDraining State = "draining" // replaced, still draining connections
)

// statePrecedence decides which resource wins when one name appears in more
// than one section. A warming listener and its active predecessor share a name;
// the active one is what traffic actually hits, so it is what we graph.
var statePrecedence = map[State]int{
	StateActive:   0,
	StateStatic:   1,
	StateWarming:  2,
	StateDraining: 3,
}

// Resource is a single xDS resource lifted out of a config dump.
type Resource struct {
	Kind  Kind
	Name  string
	State State

	// VersionInfo is the xDS version the resource arrived with. Empty for
	// static resources.
	VersionInfo string

	// LastUpdated is when Envoy last applied this resource. Zero if absent.
	LastUpdated time.Time

	// TypeURL is the Any type URL the resource was carried in.
	TypeURL string

	// Raw is the resource's JSON exactly as Envoy emitted it. This is always
	// populated and is what the UI displays, so an unparseable resource is
	// still fully inspectable.
	Raw json.RawMessage

	// Message is the decoded resource, or nil if decoding failed. Only the
	// graph builder reads it; extension configs nested inside may have been
	// dropped by the placeholder resolver.
	Message proto.Message

	// DecodeErr records why Message is nil.
	DecodeErr error

	// ErrorState is the NACK detail from the last rejected update for this
	// resource, if Envoy reported one. Its presence means what is graphed is
	// older than what the control plane most recently sent.
	ErrorState *ErrorState
}

// ErrorState describes a rejected xDS update.
type ErrorState struct {
	Details           string    `json:"details"`
	FailedVersionInfo string    `json:"failed_version_info"`
	LastUpdateAttempt time.Time `json:"last_update_attempt"`
}

type resourceKey struct {
	kind Kind
	name string
}

// Index is a name-addressable view of one config dump.
type Index struct {
	// Bootstrap is the raw bootstrap JSON, or nil if the dump omitted it.
	Bootstrap json.RawMessage

	// Raw is the complete config dump as fetched.
	Raw json.RawMessage

	// FetchedAt is when the dump was retrieved.
	FetchedAt time.Time

	all    []*Resource
	byKind map[Kind][]*Resource
	byName map[resourceKey]*Resource
	warns  []string
}

func newIndex() *Index {
	return &Index{
		byKind: make(map[Kind][]*Resource),
		byName: make(map[resourceKey]*Resource),
	}
}

// add files a resource, keeping the highest-precedence entry for each
// (kind, name) as the one lookups resolve to.
func (ix *Index) add(r *Resource) {
	ix.all = append(ix.all, r)
	ix.byKind[r.Kind] = append(ix.byKind[r.Kind], r)

	k := resourceKey{r.Kind, r.Name}
	prev, ok := ix.byName[k]
	if !ok || statePrecedence[r.State] < statePrecedence[prev.State] {
		ix.byName[k] = r
	}
}

// Get returns the effective resource for a kind and name, or nil.
func (ix *Index) Get(kind Kind, name string) *Resource {
	return ix.byName[resourceKey{kind, name}]
}

// OfKind returns every resource of a kind, including shadowed states, sorted by
// name then state precedence.
func (ix *Index) OfKind(kind Kind) []*Resource {
	out := append([]*Resource(nil), ix.byKind[kind]...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return statePrecedence[out[i].State] < statePrecedence[out[j].State]
	})
	return out
}

// All returns every resource in the dump, in discovery order.
func (ix *Index) All() []*Resource { return ix.all }

// Effective returns the winning resource for each distinct name of a kind,
// sorted by name. This is what the graph is built from: one node per listener,
// not one per lifecycle state.
func (ix *Index) Effective(kind Kind) []*Resource {
	var out []*Resource
	for k, r := range ix.byName {
		if k.kind == kind {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// OtherStates returns the states a name appears in besides the effective one,
// such as a warming update sitting behind the active listener.
func (ix *Index) OtherStates(kind Kind, name string) []State {
	eff := ix.Get(kind, name)
	if eff == nil {
		return nil
	}
	var out []State
	for _, r := range ix.byKind[kind] {
		if r.Name == name && r != eff {
			out = append(out, r.State)
		}
	}
	return out
}

// Warnings returns non-fatal problems encountered while parsing, such as
// resources that could not be decoded into typed messages.
func (ix *Index) Warnings() []string { return ix.warns }

func (ix *Index) warnf(format string, args ...any) {
	ix.warns = append(ix.warns, fmt.Sprintf(format, args...))
}

// Counts returns the number of resources per kind, counting each distinct name
// once regardless of how many lifecycle states it appears in.
func (ix *Index) Counts() map[Kind]int {
	counts := make(map[Kind]int, len(Kinds))
	for k := range ix.byName {
		counts[k.kind]++
	}
	return counts
}
