package envoytest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// DefaultVersion is the Envoy release the scenarios run against.
//
// This is pinned rather than resolved to "latest" at run time on purpose. A
// test suite whose baseline silently moves cannot distinguish "our parser
// broke" from "Envoy changed its output", and that distinction is the entire
// value of these tests. TestPinnedVersionIsCurrent checks the pin against the
// newest release so it is bumped deliberately, with the diff visible in the
// commit that bumps it.
const DefaultVersion = "v1.39.1"

// defaultRepo is the official multi-arch Envoy image.
const defaultRepo = "envoyproxy/envoy"

// Environment overrides for the image under test.
const (
	EnvVersion = "ENVOY_VERSION"
	EnvImage   = "ENVOY_IMAGE"
)

// Version returns the Envoy release to test against.
func Version() string {
	if v := os.Getenv(EnvVersion); v != "" {
		return v
	}
	return DefaultVersion
}

// Image returns the container image to run.
func Image() string {
	if img := os.Getenv(EnvImage); img != "" {
		return img
	}
	return defaultRepo + ":" + Version()
}

// latestReleaseURL is the GitHub API endpoint for the newest Envoy release.
// GitHub excludes prereleases from this endpoint, which is what we want: the
// pin should track stable releases, not release candidates.
const latestReleaseURL = "https://api.github.com/repos/envoyproxy/envoy/releases/latest"

// LatestRelease reports the newest stable Envoy release tag, e.g. "v1.39.1".
// It reaches the network, so callers must gate it behind ENVOY_TEST_ONLINE.
func LatestRelease(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest Envoy release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest Envoy release: %s", resp.Status)
	}

	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("decode latest Envoy release: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("latest Envoy release has no tag_name")
	}
	return rel.TagName, nil
}

// SemVer is a parsed Envoy release tag. Envoy tags are always three numeric
// components prefixed with "v", so this deliberately does not implement the
// full semver grammar.
type SemVer struct {
	Major, Minor, Patch int
}

// ParseVersion parses a tag such as "v1.39.1".
func ParseVersion(tag string) (SemVer, error) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return SemVer{}, fmt.Errorf("parse Envoy version %q: want three components", tag)
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return SemVer{}, fmt.Errorf("parse Envoy version %q: %w", tag, err)
		}
		out[i] = n
	}
	return SemVer{out[0], out[1], out[2]}, nil
}

func (v SemVer) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare orders two releases: -1 if v is older than w, +1 if newer, 0 if equal.
func (v SemVer) Compare(w SemVer) int {
	switch {
	case v.Major != w.Major:
		return sign(v.Major - w.Major)
	case v.Minor != w.Minor:
		return sign(v.Minor - w.Minor)
	default:
		return sign(v.Patch - w.Patch)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
