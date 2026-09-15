// Package server wires the API and the web UI onto one listener, so the whole
// tool is a single binary on a single port.
package server

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"
)

// New returns the root handler: the API under /api/, the UI everywhere else.
func New(api http.Handler, ui fs.FS, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	mux.Handle("/", spaHandler(ui))
	return logRequests(log, mux)
}

// spaHandler serves static files, falling back to index.html so client-side
// routes survive a reload or a shared link.
//
// Files are served directly rather than through http.FileServer, which would
// 301 "/index.html" to "/" and would expose directory listings.
func spaHandler(ui fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}

		info, err := fs.Stat(ui, name)
		switch {
		case err == nil && !info.IsDir():
			// Vite emits content-hashed asset filenames, so those are
			// immutable. index.html must not be cached, or a rebuilt UI keeps
			// serving stale asset references out of the browser cache.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			serveFile(w, r, ui, name)

		case err == nil || errors.Is(err, fs.ErrNotExist):
			// A missing path that looks like a file is a real 404: serving the
			// SPA shell in place of a missing script turns a build problem
			// into a baffling runtime one. Anything else is a client route.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-cache")
			serveFile(w, r, ui, "index.html")

		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, ui fs.FS, name string) {
	f, err := ui.Open(name)
	if err != nil {
		http.Error(w, "web UI is not built; run make ui", http.StatusNotFound)
		return
	}
	defer f.Close()

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Zero modtime: embedded files have no meaningful timestamp, and caching
	// is already decided by the header set above.
	http.ServeContent(w, r, path.Base(name), time.Time{}, rs)
}

// logRequests logs one line per request at debug level, and surfaces slow or
// failed requests at info.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelDebug
		if rec.status >= 500 {
			level = slog.LevelError
		} else if rec.status >= 400 {
			level = slog.LevelWarn
		}
		log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
