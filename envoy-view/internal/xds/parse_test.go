package xds

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

func loadFixture(t *testing.T, name string) *Index {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ix, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return ix
}

func TestParseStaticBasic(t *testing.T) {
	ix := loadFixture(t, "static_basic.json")

	if ix.Bootstrap == nil {
		t.Error("bootstrap section not captured")
	}

	want := map[Kind]int{KindListener: 1, KindCluster: 1}
	for kind, n := range want {
		if got := ix.Counts()[kind]; got != n {
			t.Errorf("Counts()[%s] = %d, want %d", kind, got, n)
		}
	}

	l := ix.Get(KindListener, "ingress_http")
	if l == nil {
		t.Fatal("listener ingress_http not indexed")
	}
	if l.State != StateStatic {
		t.Errorf("listener state = %s, want %s", l.State, StateStatic)
	}
	if l.VersionInfo != "" {
		t.Errorf("static listener has version %q, want empty", l.VersionInfo)
	}
	if want := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC); !l.LastUpdated.Equal(want) {
		t.Errorf("LastUpdated = %v, want %v", l.LastUpdated, want)
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener decode failed: %v", l.DecodeErr)
	}

	// The typed message must survive decoding well enough to traverse, even
	// though the Router filter's type is not linked into the binary.
	msg, ok := l.Message.(*listenerv3.Listener)
	if !ok {
		t.Fatalf("Message is %T, want *listenerv3.Listener", l.Message)
	}
	if got := msg.GetAddress().GetSocketAddress().GetPortValue(); got != 8080 {
		t.Errorf("listener port = %d, want 8080", got)
	}
	if n := len(msg.GetFilterChains()); n != 1 {
		t.Fatalf("filter chains = %d, want 1", n)
	}

	c := ix.Get(KindCluster, "service_backend")
	if c == nil {
		t.Fatal("cluster service_backend not indexed")
	}
	cmsg := c.Message.(*clusterv3.Cluster)
	if got := cmsg.GetType(); got != clusterv3.Cluster_STRICT_DNS {
		t.Errorf("cluster type = %v, want STRICT_DNS", got)
	}
	if n := len(cmsg.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()); n != 2 {
		t.Errorf("inline endpoints = %d, want 2", n)
	}
}

// Unresolvable extension types must not fail the resource that contains them.
// The placeholder drops the filter's config, but the filter itself, its name,
// and its position in the chain all survive, and the raw JSON is untouched.
func TestParseUnknownExtensionType(t *testing.T) {
	ix := loadFixture(t, "dynamic_full.json")

	l := ix.Get(KindListener, "ingress_https")
	if l == nil {
		t.Fatal("listener ingress_https not indexed")
	}
	if l.DecodeErr != nil {
		t.Fatalf("listener with unknown filter type failed to decode: %v", l.DecodeErr)
	}

	msg := l.Message.(*listenerv3.Listener)
	hcm := msg.GetFilterChains()[0].GetFilters()[0]
	if got := hcm.GetName(); got != "envoy.filters.network.http_connection_manager" {
		t.Fatalf("first filter = %q", got)
	}

	if !strings.Contains(string(l.Raw), "acme.filters.http.v1.Watermark") {
		t.Error("raw JSON lost the unknown filter; the detail pane would show nothing")
	}
	if !strings.Contains(string(l.Raw), "internal-only") {
		t.Error("raw JSON lost the unknown filter's config body")
	}
}

// Guards the fallback resolver against being deleted as apparently redundant.
// With the stock global registry this exact fixture fails to decode at all, so
// the fallback is what keeps every listener containing an unlinked filter --
// which in practice is most of them -- out of the error path.
func TestFallbackResolverIsRequired(t *testing.T) {
	ix := loadFixture(t, "dynamic_full.json")
	raw := ix.Get(KindListener, "ingress_https").Raw

	stock := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := stock.Unmarshal(raw, &listenerv3.Listener{}); err == nil {
		t.Fatal("fixture decodes without the fallback resolver; it no longer covers unknown extension types")
	}

	// And with the fallback, the types we do traverse still decode for real:
	// the HCM config must be present, not an empty placeholder.
	msg := ix.Get(KindListener, "ingress_https").Message.(*listenerv3.Listener)
	hcm := msg.GetFilterChains()[0].GetFilters()[0].GetTypedConfig()
	if len(hcm.GetValue()) == 0 {
		t.Error("HttpConnectionManager decoded to an empty placeholder; graph traversal would find nothing")
	}
}

func TestParseDynamicListenerStates(t *testing.T) {
	ix := loadFixture(t, "dynamic_full.json")

	// Both states are retained, but lookups resolve to the active one: that is
	// what traffic hits today.
	got := ix.Get(KindListener, "ingress_rollout")
	if got == nil {
		t.Fatal("ingress_rollout not indexed")
	}
	if got.State != StateActive {
		t.Errorf("effective state = %s, want %s", got.State, StateActive)
	}
	if got.VersionInfo != "2026-09-14T11:00:00Z/17" {
		t.Errorf("version = %q, want the active version", got.VersionInfo)
	}

	var states []State
	for _, r := range ix.OfKind(KindListener) {
		if r.Name == "ingress_rollout" {
			states = append(states, r.State)
		}
	}
	if len(states) != 2 {
		t.Fatalf("ingress_rollout states = %v, want active and warming", states)
	}
	if states[0] != StateActive || states[1] != StateWarming {
		t.Errorf("states = %v, want [active warming]", states)
	}
}

func TestParseListenerErrorState(t *testing.T) {
	ix := loadFixture(t, "dynamic_full.json")

	r := ix.Get(KindListener, "ingress_rejected")
	if r == nil {
		t.Fatal("ingress_rejected not indexed")
	}
	if r.ErrorState == nil {
		t.Fatal("NACK detail not captured; a stale listener would look healthy")
	}
	if !strings.Contains(r.ErrorState.Details, "duplicate filter chain match") {
		t.Errorf("details = %q", r.ErrorState.Details)
	}
	if r.ErrorState.FailedVersionInfo != "2026-09-14T11:05:00Z/18" {
		t.Errorf("failed version = %q", r.ErrorState.FailedVersionInfo)
	}
	if r.ErrorState.LastUpdateAttempt.IsZero() {
		t.Error("last_update_attempt not parsed")
	}
}

func TestParseAllKinds(t *testing.T) {
	ix := loadFixture(t, "dynamic_full.json")

	want := map[Kind]int{
		KindListener: 3, // https, rollout, rejected
		KindRoute:    1, // https_route
		KindCluster:  5, // svc_v1, svc_v2, svc_v3 (warming), tcp_backend, ext_authz_cluster
		KindEndpoint: 2,
		KindSecret:   1,
	}
	counts := ix.Counts()
	for _, kind := range Kinds {
		if counts[kind] != want[kind] {
			t.Errorf("Counts()[%s] = %d, want %d", kind, counts[kind], want[kind])
		}
	}

	// An EDS cluster's endpoints are keyed by service_name, not cluster name.
	if ix.Get(KindEndpoint, "outbound|8080|v1|svc.ns.svc.cluster.local") == nil {
		t.Error("endpoint config keyed by service_name not indexed")
	}
	// ...and by cluster name when service_name is unset.
	if ix.Get(KindEndpoint, "svc_v2") == nil {
		t.Error("endpoint config keyed by cluster name not indexed")
	}

	if w := ix.Get(KindCluster, "svc_v3"); w == nil || w.State != StateWarming {
		t.Errorf("svc_v3 = %v, want a warming cluster", w)
	}
}

// Envoy emits proto field names, but protojson with field-name preservation off
// emits lowerCamelCase. Both must parse.
func TestParseCamelCaseFieldNames(t *testing.T) {
	ix := loadFixture(t, "camel_case.json")

	l := ix.Get(KindListener, "camel_listener")
	if l == nil {
		t.Fatal("camel_listener not indexed")
	}
	if l.State != StateActive {
		t.Errorf("state = %s, want active", l.State)
	}
	if l.VersionInfo != "7" {
		t.Errorf("version = %q, want 7", l.VersionInfo)
	}
	if l.LastUpdated.IsZero() {
		t.Error("lastUpdated not parsed from camelCase field")
	}
	if ix.Get(KindCluster, "camel_cluster") == nil {
		t.Error("camel_cluster not indexed")
	}
}

func TestParseRejectsNonDump(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "this is not json"},
		{"no configs", `{"stats": []}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body)); err == nil {
				t.Error("Parse succeeded, want error")
			}
		})
	}
}

func TestParseNoSpuriousWarnings(t *testing.T) {
	for _, name := range []string{"static_basic.json", "dynamic_full.json", "camel_case.json"} {
		t.Run(name, func(t *testing.T) {
			ix := loadFixture(t, name)
			if w := ix.Warnings(); len(w) != 0 {
				t.Errorf("warnings on a well-formed dump: %v", w)
			}
		})
	}
}
