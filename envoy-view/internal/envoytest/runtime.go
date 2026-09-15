package envoytest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Environment variables that steer the framework. See the package doc.
const (
	EnvRuntime = "ENVOY_TEST_RUNTIME"
	EnvRequire = "ENVOY_TEST_REQUIRE"
	EnvKeep    = "ENVOY_TEST_KEEP"
	EnvOnline  = "ENVOY_TEST_ONLINE"
)

// runtimeCandidates are tried in order. Docker first because it is the most
// common; the rest are CLI-compatible for the handful of verbs used here.
var runtimeCandidates = []string{"docker", "podman", "nerdctl", "finch"}

// extraSearchDirs are checked after PATH. A container runtime installed by
// Homebrew or Docker Desktop frequently is not on the PATH of a non-login
// shell, and "no runtime found" on a machine that plainly has one is a bad
// first experience.
var extraSearchDirs = []string{
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"/Applications/Docker.app/Contents/Resources/bin",
}

// probeTimeout bounds the runtime health check. Docker Desktop and a colima VM
// can both take seconds to answer the first request after waking.
const probeTimeout = 20 * time.Second

// Runtime is a container runtime this machine can actually run containers with.
type Runtime struct {
	// Name is the command's base name: "docker", "podman", ...
	Name string
	// Path is the absolute path to the binary.
	Path string
}

var (
	findOnce sync.Once
	findRT   *Runtime
	findErr  error
)

// FindRuntime locates a working container runtime, or reports why it could
// not. The result is cached: probing costs a subprocess and every scenario in
// a package would otherwise pay it.
func FindRuntime() (*Runtime, error) {
	findOnce.Do(func() { findRT, findErr = findRuntime() })
	return findRT, findErr
}

// NoRuntimeError explains what was tried, so the skip message is actionable
// rather than just "no runtime".
type NoRuntimeError struct {
	Tried []string
}

func (e *NoRuntimeError) Error() string {
	if len(e.Tried) == 0 {
		return "no container runtime configured"
	}
	return "no working container runtime: " + strings.Join(e.Tried, "; ")
}

func findRuntime() (*Runtime, error) {
	candidates := runtimeCandidates
	if want := os.Getenv(EnvRuntime); want != "" {
		candidates = []string{want}
	}

	var tried []string
	for _, name := range candidates {
		path, err := lookRuntime(name)
		if err != nil {
			tried = append(tried, name+": not found")
			continue
		}
		if err := probe(path); err != nil {
			tried = append(tried, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		return &Runtime{Name: name, Path: path}, nil
	}
	return nil, &NoRuntimeError{Tried: tried}
}

// lookRuntime resolves name against PATH, then against extraSearchDirs.
func lookRuntime(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if err := isExecutable(name); err != nil {
			return "", err
		}
		return name, nil
	}
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	for _, dir := range extraSearchDirs {
		path := filepath.Join(dir, name)
		if err := isExecutable(path); err == nil {
			return path, nil
		}
	}
	return "", exec.ErrNotFound
}

func isExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return nil
}

// probe checks that the runtime can actually reach its daemon. A present
// binary proves nothing: an installed docker CLI with no daemon behind it is
// the single most common way these tests fail, and it should read as "skipped,
// daemon not running" rather than as a mysterious error mid-scenario.
func probe(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "info")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%q did not respond within %s", path+" info", probeTimeout)
		}
		return fmt.Errorf("%s: %s", err, firstLine(stderr.String()))
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	if s == "" {
		return "no output"
	}
	return s
}

// run executes a runtime subcommand and returns its stdout.
func (r *Runtime) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			r.Name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runCombined returns stdout and stderr interleaved.
//
// `logs` is the reason this exists: the runtime replays a container's stderr
// on its own stderr, and Envoy logs exclusively to stderr, so reading only
// stdout returns nothing for the one command whose output matters most when a
// scenario fails.
func (r *Runtime) runCombined(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.Path, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}
