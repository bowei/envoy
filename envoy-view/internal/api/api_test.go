package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/snapshot"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "xds", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	s := &Server{Store: snapshot.NewStore(4, time.Hour)}
	return s.Routes()
}

// upload posts a dump and returns its snapshot metadata.
func upload(t *testing.T, h http.Handler, dump []byte) Meta {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshots", bytes.NewReader(dump))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/snapshots = %d, want 201: %s", rec.Code, rec.Body)
	}
	var meta Meta
	decode(t, rec, &meta)
	return meta
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, rec.Body)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestCreateSnapshotFromUpload(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	if meta.ID == "" {
		t.Error("no snapshot id returned")
	}
	if meta.Source != "upload" {
		t.Errorf("source = %q, want upload", meta.Source)
	}
	if meta.Counts["cluster"] != 5 {
		t.Errorf("cluster count = %d, want 5", meta.Counts["cluster"])
	}
	if len(meta.Listeners) != 3 {
		t.Fatalf("listeners = %d, want 3", len(meta.Listeners))
	}

	// The root picker needs an address and a health hint per listener without
	// having to build a graph first.
	byName := map[string]ListenerSummary{}
	for _, l := range meta.Listeners {
		byName[l.Name] = l
	}
	if got := byName["ingress_https"].Address; got != "0.0.0.0:8443" {
		t.Errorf("address = %q", got)
	}
	if got := byName["ingress_rejected"].Status; got != "error" {
		t.Errorf("rejected listener status = %q, want error", got)
	}
	if got := byName["ingress_rollout"].Status; got != "warning" {
		t.Errorf("warming listener status = %q, want warning", got)
	}
}

func TestCreateSnapshotRejectsGarbage(t *testing.T) {
	h := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/snapshots", bytes.NewReader([]byte(`{"nope":1}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}

// Without an admin client and without a body there is nothing to snapshot, and
// the error has to say what to do about it.
func TestCreateSnapshotWithoutClientOrBody(t *testing.T) {
	h := newTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/snapshots", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	decode(t, rec, &body)
	if body["error"] == "" {
		t.Error("no error message")
	}
}

func TestCreateSnapshotRejectsOversizedUpload(t *testing.T) {
	s := &Server{Store: snapshot.NewStore(4, time.Hour), MaxUploadBytes: 32}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshots", bytes.NewReader(fixture(t, "static_basic.json")))
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestGraphEndpoint(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	rec := get(t, h, "/api/snapshots/"+meta.ID+"/graph?root=ingress_https")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var g graph.Graph
	decode(t, rec, &g)

	if len(g.Roots) != 1 || g.Roots[0] != "listener/ingress_https" {
		t.Errorf("roots = %v", g.Roots)
	}
	if len(g.Nodes) == 0 || len(g.Edges) == 0 {
		t.Fatalf("empty graph: %d nodes, %d edges", len(g.Nodes), len(g.Edges))
	}
	if len(g.Problems) == 0 {
		t.Error("dangling references not reported")
	}

	// The graph carries topology only; raw resource JSON is fetched per node.
	for _, n := range g.Nodes {
		if n.Kind == graph.NodeCluster && n.Resource == nil {
			t.Errorf("cluster node %s has no resource ref to fetch detail with", n.ID)
		}
	}
}

func TestGraphRootFilter(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	scoped := get(t, h, "/api/snapshots/"+meta.ID+"/graph?root=ingress_rollout")
	all := get(t, h, "/api/snapshots/"+meta.ID+"/graph")

	var a, b graph.Graph
	decode(t, scoped, &a)
	decode(t, all, &b)

	if len(a.Nodes) >= len(b.Nodes) {
		t.Errorf("rooted graph has %d nodes, unrooted %d; rooting should narrow", len(a.Nodes), len(b.Nodes))
	}
	if len(b.Roots) != 3 {
		t.Errorf("unrooted graph roots = %d, want every listener", len(b.Roots))
	}
}

func TestGraphUnknownRoot(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	rec := get(t, h, "/api/snapshots/"+meta.ID+"/graph?root=nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestResourceEndpoint(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	rec := get(t, h, "/api/snapshots/"+meta.ID+"/resource?kind=cluster&name=svc_v1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var d ResourceDetail
	decode(t, rec, &d)

	if d.Name != "svc_v1" || d.Kind != "cluster" {
		t.Errorf("got %s/%s", d.Kind, d.Name)
	}
	if d.VersionInfo == "" || d.LastUpdated == nil {
		t.Error("dump metadata missing from resource detail")
	}

	// Raw JSON must be exactly what Envoy reported, including fields no part
	// of this program understands.
	var raw map[string]any
	if err := json.Unmarshal(d.Raw, &raw); err != nil {
		t.Fatalf("raw is not valid JSON: %v", err)
	}
	if raw["@type"] == nil {
		t.Error("raw resource lost its @type")
	}
	if raw["circuit_breakers"] == nil {
		t.Error("raw resource lost fields the graph does not model")
	}
}

// Names routinely contain characters that are awkward in a path segment, which
// is why kind and name are query parameters.
func TestResourceEndpointHandlesAwkwardNames(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	name := "outbound|8080|v1|svc.ns.svc.cluster.local"
	req := httptest.NewRequest(http.MethodGet, "/api/snapshots/"+meta.ID+"/resource", nil)
	q := req.URL.Query()
	q.Set("kind", "endpoint")
	q.Set("name", name)
	req.URL.RawQuery = q.Encode()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var d ResourceDetail
	decode(t, rec, &d)
	if d.Name != name {
		t.Errorf("name = %q, want %q", d.Name, name)
	}
}

func TestResourceEndpointErrors(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"missing params", "", http.StatusBadRequest},
		{"unknown name", "?kind=cluster&name=nope", http.StatusNotFound},
		{"unknown kind", "?kind=widget&name=svc_v1", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, h, "/api/snapshots/"+meta.ID+"/resource"+tc.query)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestIndexEndpoint(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "dynamic_full.json"))

	rec := get(t, h, "/api/snapshots/"+meta.ID+"/index")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Resources []IndexEntry `json:"resources"`
	}
	decode(t, rec, &body)

	// Every lifecycle state is listed, so search can find a warming resource
	// that the graph does not draw.
	var warming bool
	for _, e := range body.Resources {
		if e.Name == "ingress_rollout" && e.State == "warming" {
			warming = true
		}
	}
	if !warming {
		t.Error("index omits shadowed lifecycle states")
	}
	if len(body.Resources) < 12 {
		t.Errorf("index has %d entries, want every resource in the dump", len(body.Resources))
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "static_basic.json"))

	if rec := get(t, h, "/api/snapshots/"+meta.ID); rec.Code != http.StatusOK {
		t.Fatalf("GET snapshot = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/snapshots/"+meta.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", rec.Code)
	}

	if rec := get(t, h, "/api/snapshots/"+meta.ID); rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete = %d, want 404", rec.Code)
	}
}

// The UI calls this on load to find a snapshot preloaded with -dump-file, which
// it has no other way to discover.
func TestListSnapshots(t *testing.T) {
	h := newTestServer(t)

	var body struct {
		Snapshots []Meta `json:"snapshots"`
	}
	decode(t, get(t, h, "/api/snapshots"), &body)
	if len(body.Snapshots) != 0 {
		t.Fatalf("empty store listed %d snapshots", len(body.Snapshots))
	}

	first := upload(t, h, fixture(t, "static_basic.json"))
	second := upload(t, h, fixture(t, "dynamic_full.json"))

	decode(t, get(t, h, "/api/snapshots"), &body)
	if len(body.Snapshots) != 2 {
		t.Fatalf("listed %d snapshots, want 2", len(body.Snapshots))
	}
	// Newest first, so the UI can open [0] without sorting.
	if body.Snapshots[0].ID != second.ID || body.Snapshots[1].ID != first.ID {
		t.Errorf("snapshots are not newest-first: got %s then %s",
			body.Snapshots[0].ID, body.Snapshots[1].ID)
	}
	if len(body.Snapshots[0].Listeners) == 0 {
		t.Error("listing carries no listeners; the UI cannot populate its picker")
	}
}

// An expired snapshot must be distinguishable from a bad URL, because the UI
// turns it into a "re-fetch" prompt.
func TestExpiredSnapshot(t *testing.T) {
	s := &Server{Store: snapshot.NewStore(4, time.Nanosecond)}
	h := s.Routes()
	meta := upload(t, h, fixture(t, "static_basic.json"))

	time.Sleep(2 * time.Millisecond)

	rec := get(t, h, "/api/snapshots/"+meta.ID+"/graph")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	decode(t, rec, &body)
	if body["error"] == "" {
		t.Error("expiry returned no explanation")
	}
}

func TestConfigEndpoint(t *testing.T) {
	h := newTestServer(t)
	rec := get(t, h, "/api/config")

	var cfg Config
	decode(t, rec, &cfg)
	if cfg.CanFetch {
		t.Error("CanFetch is true with no admin client configured")
	}
}

func TestResponsesAreNotCacheable(t *testing.T) {
	h := newTestServer(t)
	meta := upload(t, h, fixture(t, "static_basic.json"))

	rec := get(t, h, "/api/snapshots/"+meta.ID)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
