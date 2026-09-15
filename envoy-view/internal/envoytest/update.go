package envoytest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/boweidu/envoy-view/internal/xds"
)

// UpdateTimeout bounds how long WaitFor will poll for Envoy to converge.
const UpdateTimeout = 30 * time.Second

// WriteFile replaces a file in the container's config directory.
//
// This is how a filesystem-xDS scenario pushes a new discovery response. The
// write is done as a copy to a temporary name followed by a rename, because
// Envoy's filesystem subscription watches the directory for a completed move:
// a plain overwrite can be observed half-written, and Envoy would then reject
// a truncated resource for reasons that have nothing to do with the test.
func (e *Envoy) WriteFile(name, content string) {
	e.tb.Helper()
	if strings.ContainsRune(name, '/') {
		e.tb.Fatalf("envoytest: WriteFile name %q must be a base name within %s", name, ConfigDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	host := filepath.Join(e.tb.TempDir(), name)
	if err := os.WriteFile(host, []byte(content), 0o644); err != nil {
		e.tb.Fatalf("envoytest: write %s: %v", host, err)
	}

	staged := filepath.Join(ConfigDir, "."+name+".staged")
	final := filepath.Join(ConfigDir, name)
	if _, err := e.rt.run(ctx, "cp", host, fmt.Sprintf("%s:%s", e.id, staged)); err != nil {
		e.tb.Fatalf("envoytest: stage %s in %s: %v", name, e.name, err)
	}
	// -u 0 because ConfigDir is root-owned and Envoy runs unprivileged.
	if _, err := e.rt.run(ctx, "exec", "-u", "0", e.id, "mv", staged, final); err != nil {
		e.tb.Fatalf("envoytest: commit %s in %s: %v", name, e.name, err)
	}
}

// WaitFor polls the config dump until cond holds, and returns the index that
// satisfied it.
//
// xDS is eventually consistent even over the filesystem: the write returns
// before Envoy has read, validated, and applied the update. Asserting straight
// after a write is the classic flaky test, so scenarios say what they are
// waiting for instead.
func (e *Envoy) WaitFor(desc string, cond func(*xds.Index) bool) *xds.Index {
	e.tb.Helper()
	deadline := time.Now().Add(UpdateTimeout)

	var last *xds.Index
	for {
		ix, err := xds.Parse(e.ConfigDump())
		if err == nil {
			last = ix
			if cond(ix) {
				return ix
			}
		}
		if time.Now().After(deadline) {
			e.tb.Fatalf("envoytest: scenario %q: timed out after %s waiting for %s\n%s",
				e.cfg.Name, UpdateTimeout, desc, summarize(last))
			return nil
		}
		time.Sleep(pollInterval)
	}
}

// summarize renders what was actually in the dump when a wait gave up, so the
// failure says "here is what Envoy had" rather than only "it never happened".
func summarize(ix *xds.Index) string {
	if ix == nil {
		return "last config dump: <none parsed>"
	}
	var b strings.Builder
	b.WriteString("last config dump held:\n")
	for _, kind := range xds.Kinds {
		for _, r := range ix.OfKind(kind) {
			fmt.Fprintf(&b, "  %-9s %-40s %-9s version=%q", r.Kind, r.Name, r.State, r.VersionInfo)
			if r.ErrorState != nil {
				fmt.Fprintf(&b, " rejected=%q", firstLine(r.ErrorState.Details))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}
