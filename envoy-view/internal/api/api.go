// Package api serves the JSON endpoints the UI reads.
//
// The shape follows from one constraint: a production config dump is far too
// large to hand to a browser. So a snapshot is parsed once and kept server-side
// under an ID, the graph endpoint returns topology only, and full resource JSON
// is fetched one resource at a time as the user clicks.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/boweidu/envoy-view/internal/admin"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/snapshot"
	"github.com/boweidu/envoy-view/internal/xds"
)

// DefaultMaxUploadBytes caps an uploaded config dump. Real dumps run to tens of
// megabytes; this leaves generous headroom without letting one request exhaust
// memory.
const DefaultMaxUploadBytes = 256 << 20

// Server implements the API.
type Server struct {
	// Client fetches live dumps. Nil means upload-only mode.
	Client *admin.Client

	Store *snapshot.Store
	Log   *slog.Logger

	// IncludeEDS asks Envoy for endpoint data on live fetches.
	IncludeEDS bool

	// MaxUploadBytes bounds an uploaded dump; zero uses the default.
	MaxUploadBytes int64
}

// Routes returns the API handler, to be mounted at /api/.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/snapshots", s.handleListSnapshots)
	mux.HandleFunc("POST /api/snapshots", s.handleCreateSnapshot)
	mux.HandleFunc("GET /api/snapshots/{id}", s.handleGetSnapshot)
	mux.HandleFunc("DELETE /api/snapshots/{id}", s.handleDeleteSnapshot)
	mux.HandleFunc("GET /api/snapshots/{id}/graph", s.handleGraph)
	mux.HandleFunc("GET /api/snapshots/{id}/index", s.handleIndex)
	mux.HandleFunc("GET /api/snapshots/{id}/resource", s.handleResource)
	return mux
}

// ---------------------------------------------------------------- responses

// Config tells the UI what this server can do.
type Config struct {
	// EnvoyAdmin is the address live fetches go to, empty in upload-only mode.
	EnvoyAdmin string `json:"envoyAdmin"`
	CanFetch   bool   `json:"canFetch"`
	IncludeEDS bool   `json:"includeEds"`
}

// ListenerSummary is what the root picker needs, without loading a graph.
type ListenerSummary struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	State   string `json:"state"`
	Status  string `json:"status"`
}

// Meta describes a stored snapshot.
type Meta struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	CreatedAt time.Time         `json:"createdAt"`
	SizeBytes int               `json:"sizeBytes"`
	Counts    map[string]int    `json:"counts"`
	Warnings  []string          `json:"warnings"`
	Listeners []ListenerSummary `json:"listeners"`

	ServerInfo *admin.ServerInfo `json:"serverInfo,omitempty"`
}

// IndexEntry is one searchable resource name.
type IndexEntry struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// ResourceDetail is a single resource with its dump metadata and raw JSON.
type ResourceDetail struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	State       string          `json:"state"`
	VersionInfo string          `json:"versionInfo,omitempty"`
	LastUpdated *time.Time      `json:"lastUpdated,omitempty"`
	TypeURL     string          `json:"typeUrl,omitempty"`
	DecodeError string          `json:"decodeError,omitempty"`
	ErrorState  *xds.ErrorState `json:"errorState,omitempty"`
	Raw         json.RawMessage `json:"raw"`
}

// ----------------------------------------------------------------- handlers

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := Config{CanFetch: s.Client != nil, IncludeEDS: s.IncludeEDS}
	if s.Client != nil {
		cfg.EnvoyAdmin = s.Client.Addr()
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleListSnapshots lets the UI discover what is already loaded on startup:
// a dump preloaded with -dump-file, or the snapshot from a previous page load.
// Without this the UI could only ever see snapshots it created itself.
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	metas := []Meta{}
	for _, snap := range s.Store.List() {
		metas = append(metas, metaOf(snap))
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": metas})
}

// handleCreateSnapshot fetches a dump from Envoy, or parses one uploaded in the
// request body. Accepting an upload is what makes the tool usable against a
// support bundle or a CI artifact, with no reachable Envoy at all.
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var (
		raw    []byte
		source string
		err    error
	)

	if r.ContentLength != 0 {
		max := s.MaxUploadBytes
		if max <= 0 {
			max = DefaultMaxUploadBytes
		}
		raw, err = io.ReadAll(http.MaxBytesReader(w, r.Body, max))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				s.fail(w, http.StatusRequestEntityTooLarge,
					fmt.Errorf("config dump exceeds the %d byte limit", max))
				return
			}
			s.fail(w, http.StatusBadRequest, fmt.Errorf("read uploaded dump: %w", err))
			return
		}
		source = "upload"
	} else {
		if s.Client == nil {
			s.fail(w, http.StatusBadRequest,
				errors.New("no Envoy admin address is configured; upload a config dump in the request body"))
			return
		}
		raw, err = s.Client.ConfigDump(r.Context(), s.IncludeEDS)
		if err != nil {
			s.fail(w, http.StatusBadGateway, err)
			return
		}
		source = s.Client.Addr()
	}

	index, err := xds.Parse(raw)
	if err != nil {
		s.fail(w, http.StatusUnprocessableEntity, err)
		return
	}

	snap := &snapshot.Snapshot{Source: source, SizeBytes: len(raw), Index: index}
	// Envoy's self-report is useful context but not worth failing the snapshot
	// over: /server_info can be unavailable while /config_dump works.
	if s.Client != nil && source != "upload" {
		if info, err := s.Client.ServerInfo(r.Context()); err == nil {
			snap.ServerInfo = info
		} else {
			s.logger().Debug("server_info unavailable", "error", err)
		}
	}

	stored, err := s.Store.Put(snap)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, metaOf(stored))
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.lookup(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, metaOf(snap))
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	s.Store.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.lookup(w, r)
	if !ok {
		return
	}

	opts := graph.Options{}
	if root := r.URL.Query().Get("root"); root != "" {
		opts.Roots = []string{root}
		if snap.Index.Get(xds.KindListener, root) == nil {
			s.fail(w, http.StatusNotFound, fmt.Errorf("no listener named %q in this snapshot", root))
			return
		}
	} else {
		// Whole-config view: unreferenced clusters are worth seeing, since a
		// cluster nothing routes to is usually the other half of a bug.
		opts.IncludeOrphanClusters = true
	}
	if v := r.URL.Query().Get("orphans"); v != "" {
		opts.IncludeOrphanClusters = v == "1" || v == "true"
	}

	writeJSON(w, http.StatusOK, graph.Build(snap.Index, opts))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.lookup(w, r)
	if !ok {
		return
	}

	entries := []IndexEntry{}
	for _, kind := range xds.Kinds {
		for _, res := range snap.Index.OfKind(kind) {
			entries = append(entries, IndexEntry{
				Kind:  string(res.Kind),
				Name:  res.Name,
				State: string(res.State),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": entries})
}

// handleResource returns one resource's raw JSON. Kind and name come from the
// query string rather than the path because xDS names routinely contain
// characters that are painful in a path segment, such as Istio's
// "outbound|8080|v1|svc.ns.svc.cluster.local".
func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.lookup(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	kind := xds.Kind(q.Get("kind"))
	name := q.Get("name")
	if kind == "" || name == "" {
		s.fail(w, http.StatusBadRequest, errors.New("kind and name query parameters are required"))
		return
	}

	res := snap.Index.Get(kind, name)
	if res == nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("no %s named %q in this snapshot", kind, name))
		return
	}

	detail := ResourceDetail{
		Kind:        string(res.Kind),
		Name:        res.Name,
		State:       string(res.State),
		VersionInfo: res.VersionInfo,
		TypeURL:     res.TypeURL,
		ErrorState:  res.ErrorState,
		Raw:         res.Raw,
	}
	if !res.LastUpdated.IsZero() {
		t := res.LastUpdated
		detail.LastUpdated = &t
	}
	if res.DecodeErr != nil {
		detail.DecodeError = res.DecodeErr.Error()
	}
	writeJSON(w, http.StatusOK, detail)
}

// ------------------------------------------------------------------ helpers

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*snapshot.Snapshot, bool) {
	id := r.PathValue("id")
	snap, ok := s.Store.Get(id)
	if !ok {
		// Expiry is the common case here, so say so: the UI turns this into a
		// "refresh to re-fetch" prompt rather than a bare 404.
		s.fail(w, http.StatusNotFound, fmt.Errorf("snapshot %q is unknown or has expired", id))
		return nil, false
	}
	return snap, true
}

func metaOf(snap *snapshot.Snapshot) Meta {
	counts := make(map[string]int, len(xds.Kinds))
	for kind, n := range snap.Index.Counts() {
		counts[string(kind)] = n
	}
	for _, kind := range xds.Kinds {
		if _, ok := counts[string(kind)]; !ok {
			counts[string(kind)] = 0
		}
	}

	listeners := []ListenerSummary{}
	for _, res := range snap.Index.Effective(xds.KindListener) {
		l := ListenerSummary{
			Name:   res.Name,
			State:  string(res.State),
			Status: "ok",
		}
		if res.ErrorState != nil || res.DecodeErr != nil {
			l.Status = "error"
		} else if len(snap.Index.OtherStates(xds.KindListener, res.Name)) > 0 {
			l.Status = "warning"
		}
		l.Address = graph.ListenerAddress(res)
		listeners = append(listeners, l)
	}

	warnings := snap.Index.Warnings()
	if warnings == nil {
		warnings = []string{}
	}

	return Meta{
		ID:         snap.ID,
		Source:     snap.Source,
		CreatedAt:  snap.CreatedAt,
		SizeBytes:  snap.SizeBytes,
		Counts:     counts,
		Warnings:   warnings,
		Listeners:  listeners,
		ServerInfo: snap.ServerInfo,
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		s.logger().Error("api error", "status", code, "error", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		slog.Error("encode response", "error", err)
		http.Error(w, `{"error":"failed to encode response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The admin interface data this exposes should never be cached by an
	// intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}
