// Package webui carries the built web UI so envoy-view ships as one binary.
//
// dist/ is Vite output and is committed, which keeps "go install" working for
// people who have a Go toolchain and no npm. Rebuild it with "make ui" (or
// "npm run build" in web/) after changing anything under web/src.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI rooted at dist/.
func FS() (fs.FS, error) {
	return fs.Sub(dist, "dist")
}
