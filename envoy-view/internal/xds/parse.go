package xds

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Parse turns the JSON body of Envoy's /config_dump into an Index.
//
// The dump is walked as generic JSON rather than unmarshaled into
// envoy.admin.v3.ConfigDump in one shot. Two reasons:
//
//   - Each resource's original JSON is preserved verbatim for display, so the
//     UI can always show exactly what Envoy reported even for extensions this
//     binary knows nothing about.
//   - Decoding is per-resource, so one undecodable listener costs that listener
//     rather than the whole dump.
//
// Envoy emits protojson, which may use either the proto field names or their
// lowerCamelCase forms depending on version and flags, so every field lookup
// accepts both.
func Parse(raw []byte) (*Index, error) {
	var top struct {
		Configs []json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("parse config dump: %w", err)
	}
	if len(top.Configs) == 0 {
		return nil, fmt.Errorf("config dump has no \"configs\" array; is this an Envoy /config_dump response?")
	}

	ix := newIndex()
	ix.Raw = append(json.RawMessage(nil), raw...)
	ix.FetchedAt = time.Now()

	for i, sectionRaw := range top.Configs {
		section, err := asObject(sectionRaw)
		if err != nil {
			ix.warnf("configs[%d]: %v", i, err)
			continue
		}
		typeURL := stringField(section, "@type")
		switch shortTypeName(typeURL) {
		case "envoy.admin.v3.BootstrapConfigDump":
			ix.Bootstrap, _ = section.get("bootstrap")

		case "envoy.admin.v3.ListenersConfigDump":
			ix.parseStatic(section, KindListener, "static_listeners", "listener")
			ix.parseDynamicListeners(section)

		case "envoy.admin.v3.ClustersConfigDump":
			ix.parseStatic(section, KindCluster, "static_clusters", "cluster")
			ix.parseDynamic(section, KindCluster, "dynamic_active_clusters", "cluster", StateActive)
			ix.parseDynamic(section, KindCluster, "dynamic_warming_clusters", "cluster", StateWarming)

		case "envoy.admin.v3.RoutesConfigDump":
			ix.parseStatic(section, KindRoute, "static_route_configs", "route_config")
			ix.parseDynamic(section, KindRoute, "dynamic_route_configs", "route_config", StateActive)

		case "envoy.admin.v3.EndpointsConfigDump":
			ix.parseStatic(section, KindEndpoint, "static_endpoint_configs", "endpoint_config")
			ix.parseDynamic(section, KindEndpoint, "dynamic_endpoint_configs", "endpoint_config", StateActive)

		case "envoy.admin.v3.SecretsConfigDump":
			ix.parseStatic(section, KindSecret, "static_secrets", "secret")
			ix.parseDynamic(section, KindSecret, "dynamic_active_secrets", "secret", StateActive)
			ix.parseDynamic(section, KindSecret, "dynamic_warming_secrets", "secret", StateWarming)

		case "envoy.admin.v3.ScopedRoutesConfigDump":
			// Scoped RDS attaches route tables by scope key rather than by a
			// single name, which the graph model does not represent yet.
			//
			// Envoy emits this section as "{}" on every dump whether or not
			// scoped routes are configured, so the presence of the section
			// says nothing; only a non-empty list is worth warning about.
			if n := len(section.list("inline_scoped_route_configs")) +
				len(section.list("dynamic_scoped_route_configs")); n > 0 {
				ix.warnf("%d scoped route configuration(s) are present but not graphed", n)
			}

		case "":
			ix.warnf("configs[%d]: missing @type", i)
		}
	}

	return ix, nil
}

// parseStatic reads a list of static entries, each wrapping one resource Any.
func (ix *Index) parseStatic(section jobj, kind Kind, listField, resField string) {
	for i, entryRaw := range section.list(listField) {
		entry, err := asObject(entryRaw)
		if err != nil {
			ix.warnf("%s[%d]: %v", listField, i, err)
			continue
		}
		ix.addEntry(kind, StateStatic, entry, listField, i, resField, "")
	}
}

// parseDynamic reads a list of dynamically delivered entries.
func (ix *Index) parseDynamic(section jobj, kind Kind, listField, resField string, state State) {
	for i, entryRaw := range section.list(listField) {
		entry, err := asObject(entryRaw)
		if err != nil {
			ix.warnf("%s[%d]: %v", listField, i, err)
			continue
		}
		ix.addEntry(kind, state, entry, listField, i, resField, stringField(entry, "version_info"))
	}
}

// parseDynamicListeners handles LDS, which is shaped differently from the other
// dynamic sections: one entry per listener name, holding up to three lifecycle
// states that each carry their own copy of the listener.
func (ix *Index) parseDynamicListeners(section jobj) {
	const listField = "dynamic_listeners"
	states := []struct {
		field string
		state State
	}{
		{"active_state", StateActive},
		{"warming_state", StateWarming},
		{"draining_state", StateDraining},
	}

	for i, entryRaw := range section.list(listField) {
		entry, err := asObject(entryRaw)
		if err != nil {
			ix.warnf("%s[%d]: %v", listField, i, err)
			continue
		}
		// The listener name lives on the outer entry, and is the authority even
		// if a nested listener message somehow disagrees.
		name := stringField(entry, "name")

		for _, st := range states {
			stateRaw, ok := entry.get(st.field)
			if !ok {
				continue
			}
			stateObj, err := asObject(stateRaw)
			if err != nil {
				ix.warnf("%s[%d].%s: %v", listField, i, st.field, err)
				continue
			}
			r := ix.addEntry(KindListener, st.state, stateObj, listField, i, "listener",
				stringField(stateObj, "version_info"))
			if r == nil {
				continue
			}
			if name != "" {
				ix.rename(r, name)
			}
			// error_state sits on the outer entry, not on the per-state object.
			if r.ErrorState == nil {
				r.ErrorState = parseErrorState(entry)
			}
		}
	}
}

// addEntry lifts one resource out of a dump entry and files it in the index.
// Returns nil if the entry carried no resource.
func (ix *Index) addEntry(kind Kind, state State, entry jobj, listField string, i int, resField, versionInfo string) *Resource {
	resRaw, ok := entry.get(resField)
	if !ok {
		// A dump entry with no payload is normal for a resource that was
		// NACKed before Envoy ever applied a version of it.
		if es := parseErrorState(entry); es != nil {
			ix.warnf("%s[%d]: no %s, last update rejected: %s", listField, i, resField, es.Details)
		} else {
			ix.warnf("%s[%d]: missing %s", listField, i, resField)
		}
		return nil
	}

	resObj, err := asObject(resRaw)
	if err != nil {
		ix.warnf("%s[%d].%s: %v", listField, i, resField, err)
		return nil
	}

	r := &Resource{
		Kind:        kind,
		Name:        resourceName(kind, resObj),
		State:       state,
		VersionInfo: versionInfo,
		LastUpdated: parseTimestamp(stringField(entry, "last_updated")),
		TypeURL:     stringField(resObj, "@type"),
		Raw:         resRaw,
		ErrorState:  parseErrorState(entry),
	}
	// Static secret entries carry the name on the wrapper rather than inside
	// the secret, and dynamic secrets repeat it in both places.
	if r.Name == "" {
		r.Name = stringField(entry, "name")
	}
	if r.Name == "" {
		r.Name = fmt.Sprintf("<unnamed %s %d>", kind, i)
		ix.warnf("%s[%d]: resource has no name", listField, i)
	}

	r.Message, r.DecodeErr = decodeResource(kind, resRaw)
	if r.DecodeErr != nil {
		ix.warnf("%s %q: %v", kind, r.Name, r.DecodeErr)
	}

	ix.add(r)
	return r
}

// rename re-files a resource under a corrected name. Used by LDS, where the
// authoritative name is only known after the resource has been read.
func (ix *Index) rename(r *Resource, name string) {
	if r.Name == name {
		return
	}
	delete(ix.byName, resourceKey{r.Kind, r.Name})
	r.Name = name
	k := resourceKey{r.Kind, name}
	if prev, ok := ix.byName[k]; !ok || statePrecedence[r.State] < statePrecedence[prev.State] {
		ix.byName[k] = r
	}
}

// unmarshalOpts decodes a resource leniently: unknown fields are dropped so a
// dump from a newer Envoy still parses, and unresolvable extension types fall
// back to empty placeholders rather than failing the resource.
var unmarshalOpts = protojson.UnmarshalOptions{
	DiscardUnknown: true,
	Resolver:       newResolver(),
}

// decodeResource parses a resource's JSON into its concrete message type. The
// "@type" key present on the Any-encoded JSON is discarded as an unknown field.
func decodeResource(kind Kind, raw json.RawMessage) (proto.Message, error) {
	var msg proto.Message
	switch kind {
	case KindListener:
		msg = &listenerv3.Listener{}
	case KindCluster:
		msg = &clusterv3.Cluster{}
	case KindRoute:
		msg = &routev3.RouteConfiguration{}
	case KindEndpoint:
		msg = &endpointv3.ClusterLoadAssignment{}
	case KindSecret:
		msg = &tlsv3.Secret{}
	default:
		return nil, fmt.Errorf("unsupported kind %q", kind)
	}
	if err := unmarshalOpts.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return msg, nil
}

// resourceName pulls the identifying name out of a resource body.
func resourceName(kind Kind, obj jobj) string {
	if kind == KindEndpoint {
		// ClusterLoadAssignment keys on the cluster it serves.
		return stringField(obj, "cluster_name")
	}
	return stringField(obj, "name")
}

func parseErrorState(entry jobj) *ErrorState {
	raw, ok := entry.get("error_state")
	if !ok {
		return nil
	}
	obj, err := asObject(raw)
	if err != nil {
		return nil
	}
	return &ErrorState{
		Details:           stringField(obj, "details"),
		FailedVersionInfo: stringField(obj, "version_info"),
		LastUpdateAttempt: parseTimestamp(stringField(obj, "last_update_attempt")),
	}
}

func parseTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// shortTypeName strips the type host from an Any type URL.
func shortTypeName(typeURL string) string {
	if i := strings.LastIndexByte(typeURL, '/'); i >= 0 {
		return typeURL[i+1:]
	}
	return typeURL
}

// jobj is a lazily-decoded JSON object. Values stay as raw bytes so a
// resource's exact original encoding survives into the UI.
type jobj map[string]json.RawMessage

func asObject(raw json.RawMessage) (jobj, error) {
	var o jobj
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("expected JSON object: %w", err)
	}
	return o, nil
}

// get looks up a field by its proto name, falling back to the lowerCamelCase
// form protojson produces when field-name preservation is off.
func (o jobj) get(name string) (json.RawMessage, bool) {
	if v, ok := o[name]; ok {
		return v, true
	}
	if camel := lowerCamel(name); camel != name {
		if v, ok := o[camel]; ok {
			return v, true
		}
	}
	return nil, false
}

// list returns a field's array elements, or nil if absent or not an array.
func (o jobj) list(name string) []json.RawMessage {
	raw, ok := o.get(name)
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}

func stringField(o jobj, name string) string {
	raw, ok := o.get(name)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func lowerCamel(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	up := false
	for _, r := range s {
		switch {
		case r == '_':
			up = true
		case up:
			b.WriteRune(upperRune(r))
			up = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func upperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}
