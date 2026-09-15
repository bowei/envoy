package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	ui := fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<!doctype html><title>ui</title>")},
		"assets/app-a1b2c3.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"favicon.svg":          &fstest.MapFile{Data: []byte("<svg/>")},
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	return New(api, ui, nil)
}

func do(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	testHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestAPIIsMounted(t *testing.T) {
	rec := do(t, "/api/anything")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Errorf("api not reachable: %d %s", rec.Code, rec.Body)
	}
}

func TestServesStaticAssets(t *testing.T) {
	rec := do(t, "/assets/app-a1b2c3.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// Vite content-hashes asset names, so they are safe to cache forever.
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
}

// index.html must never be cached, or a rebuilt UI keeps serving the old asset
// references from the browser cache.
func TestIndexIsNotCached(t *testing.T) {
	for _, path := range []string{"/", "/index.html"} {
		rec := do(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// A client-side route must survive a reload or a shared link.
func TestClientRoutesFallBackToIndex(t *testing.T) {
	for _, path := range []string{"/snapshot/abc123", "/listener/ingress_http"} {
		rec := do(t, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want the SPA shell", path, rec.Code)
		}
		if rec.Body.String() != "<!doctype html><title>ui</title>" {
			t.Errorf("GET %s did not serve index.html", path)
		}
	}
}

// A missing file is a real 404, not the SPA shell: returning HTML for a missing
// script makes build problems very hard to see.
func TestMissingAssetIs404(t *testing.T) {
	for _, path := range []string{"/assets/gone.js", "/missing.css"} {
		if rec := do(t, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}
