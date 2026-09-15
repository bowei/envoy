package envoytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boweidu/envoy-view/internal/admin"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Timeouts for the container lifecycle. Startup is generous because the first
// scenario in a run may be waiting on an image pull or a cold VM.
const (
	StartTimeout    = 90 * time.Second
	ShutdownTimeout = 20 * time.Second
	pollInterval    = 100 * time.Millisecond
)

// Envoy is a running containerized Envoy.
type Envoy struct {
	tb  testing.TB
	rt  *Runtime
	cfg Config

	id   string
	name string

	// hostPorts maps a container port to the host address it is published on.
	hostPorts map[uint32]string

	client *admin.Client
}

// RequireRuntime returns a working container runtime, skipping the test when
// there is none -- unless ENVOY_TEST_REQUIRE is set, which is how CI asserts
// that the container tests really ran.
func RequireRuntime(tb testing.TB) *Runtime {
	tb.Helper()
	if testing.Short() {
		tb.Skip("skipping container scenario in -short mode")
	}
	rt, err := FindRuntime()
	if err != nil {
		if os.Getenv(EnvRequire) != "" {
			tb.Fatalf("%s is set but no container runtime is usable: %v", EnvRequire, err)
		}
		tb.Skipf("skipping: %v (set %s=1 to make this a failure)", err, EnvRequire)
	}
	return rt
}

// Start boots Envoy with cfg and waits until it reports itself live.
//
// The container is removed when the test ends. Set ENVOY_TEST_KEEP=1 to leave
// it running and print the commands to inspect it.
func Start(tb testing.TB, cfg Config) *Envoy {
	tb.Helper()
	rt := RequireRuntime(tb)

	bootstrap, err := cfg.render()
	if err != nil {
		tb.Fatalf("envoytest: %v", err)
	}

	e := &Envoy{
		tb:        tb,
		rt:        rt,
		cfg:       cfg,
		name:      containerName(cfg.Name),
		hostPorts: make(map[uint32]string),
	}

	ctx, cancel := context.WithTimeout(context.Background(), StartTimeout)
	defer cancel()

	dir := tb.TempDir()
	files := map[string]string{BootstrapFile: string(bootstrap)}
	for name, content := range cfg.Files {
		if name == BootstrapFile {
			tb.Fatalf("envoytest: scenario %q may not supply %s; set Config.Bootstrap instead",
				cfg.Name, BootstrapFile)
		}
		files[name] = content
	}

	// create-then-copy-then-start, rather than run with a bind mount: see the
	// package doc on why the files must live on the container's own filesystem.
	e.id = e.create(ctx)
	tb.Cleanup(e.stop)

	for name, content := range files {
		e.copyIn(ctx, dir, name, content)
	}

	if _, err := rt.run(ctx, "start", e.id); err != nil {
		tb.Fatalf("envoytest: start %s: %v", e.name, err)
	}

	e.discoverPorts(ctx)
	e.client = e.newClient()
	e.waitReady(ctx)
	return e
}

func containerName(scenario string) string {
	var b [4]byte
	// Errors here are not survivable and never happen; a panic is better than
	// a colliding container name that fails confusingly much later.
	if _, err := rand.Read(b[:]); err != nil {
		panic("envoytest: read random: " + err.Error())
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, scenario)
	if safe == "" {
		safe = "scenario"
	}
	return fmt.Sprintf("envoy-view-test-%s-%s", safe, hex.EncodeToString(b[:]))
}

func (e *Envoy) create(ctx context.Context) string {
	args := []string{
		"create",
		"--name", e.name,
		// The label lets a stray container from a killed test run be found and
		// removed: `docker rm -f $(docker ps -aq --filter label=envoy-view-test)`.
		"--label", "envoy-view-test=1",
		// Publishing to 127.0.0.1 with no host port lets the runtime pick a
		// free one, which is the only way parallel scenarios avoid collisions.
		"--publish", fmt.Sprintf("127.0.0.1::%d", AdminPort),
	}
	for _, p := range e.cfg.Ports {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", p))
	}

	logLevel := e.cfg.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}
	args = append(args, Image(),
		"envoy",
		"-c", filepath.Join(ConfigDir, BootstrapFile),
		"--log-level", logLevel,
	)

	id, err := e.rt.run(ctx, args...)
	if err != nil {
		e.tb.Fatalf("envoytest: create container for scenario %q: %v", e.cfg.Name, err)
	}
	return strings.TrimSpace(id)
}

// copyIn writes content to the host temp dir and copies it into the container.
func (e *Envoy) copyIn(ctx context.Context, dir, name, content string) {
	host := filepath.Join(dir, name)
	// 0644: the image's entrypoint drops to the unprivileged "envoy" user, and
	// cp preserves the host mode, so a private mode here makes the config
	// unreadable by the process that needs it.
	if err := os.WriteFile(host, []byte(content), 0o644); err != nil {
		e.tb.Fatalf("envoytest: write %s: %v", host, err)
	}
	dst := fmt.Sprintf("%s:%s", e.id, filepath.Join(ConfigDir, name))
	if _, err := e.rt.run(ctx, "cp", host, dst); err != nil {
		e.tb.Fatalf("envoytest: copy %s into %s: %v", name, e.name, err)
	}
}

// discoverPorts records the host address each published container port landed on.
func (e *Envoy) discoverPorts(ctx context.Context) {
	ports := append([]uint32{AdminPort}, e.cfg.Ports...)
	for _, p := range ports {
		out, err := e.rt.run(ctx, "port", e.id, fmt.Sprintf("%d/tcp", p))
		if err != nil {
			e.tb.Fatalf("envoytest: look up published port %d: %v", p, err)
		}
		addr, err := parsePortOutput(out)
		if err != nil {
			e.tb.Fatalf("envoytest: port %d on %s: %v", p, e.name, err)
		}
		e.hostPorts[p] = addr
	}
}

// parsePortOutput reads the host address out of `<runtime> port` output.
//
// The runtimes print one line per published address and may include an IPv6
// line such as "[::]:32771", which is not reachable when the publish spec
// asked for 127.0.0.1. The first IPv4 line is the one to use.
func parsePortOutput(out string) (string, error) {
	var first string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Some runtimes print "9901/tcp -> 127.0.0.1:32771".
		if i := strings.LastIndex(line, "-> "); i >= 0 {
			line = strings.TrimSpace(line[i+len("-> "):])
		}
		if first == "" {
			first = line
		}
		if !strings.HasPrefix(line, "[") {
			return line, nil
		}
	}
	if first == "" {
		return "", fmt.Errorf("no published address in %q", out)
	}
	return first, nil
}

func (e *Envoy) newClient() *admin.Client {
	c, err := admin.New(e.hostPorts[AdminPort])
	if err != nil {
		e.tb.Fatalf("envoytest: admin client: %v", err)
	}
	return c
}

// waitReady polls the admin /ready endpoint until Envoy reports LIVE.
//
// It also watches for the container exiting, because the common failure -- a
// config Envoy rejects -- otherwise shows up only as a timeout, minutes later,
// with the actual error sitting unread in the container log.
func (e *Envoy) waitReady(ctx context.Context) {
	e.tb.Helper()
	url := "http://" + e.hostPorts[AdminPort] + "/ready"
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			e.tb.Fatalf("envoytest: scenario %q did not become ready within %s (last: %v)\n%s",
				e.cfg.Name, StartTimeout, lastErr, e.Logs())
		}
		if !e.running(ctx) {
			e.tb.Fatalf("envoytest: scenario %q exited during startup; Envoy rejected its config\n%s",
				e.cfg.Name, e.Logs())
		}

		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
		} else {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}

		select {
		case <-ctx.Done():
		case <-time.After(pollInterval):
		}
	}
}

func (e *Envoy) running(ctx context.Context) bool {
	out, err := e.rt.run(ctx, "inspect", "-f", "{{.State.Running}}", e.id)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

func (e *Envoy) stop() {
	if os.Getenv(EnvKeep) != "" {
		e.tb.Logf("envoytest: %s left running (%s)\n  admin: http://%s\n  logs:  %s logs %s\n  rm:    %s rm -f %s",
			e.name, EnvKeep, e.hostPorts[AdminPort], e.rt.Name, e.name, e.rt.Name, e.name)
		return
	}
	if e.tb.Failed() {
		e.tb.Logf("envoytest: %s container log follows\n%s", e.cfg.Name, e.Logs())
	}

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if _, err := e.rt.run(ctx, "rm", "-f", e.id); err != nil {
		e.tb.Logf("envoytest: removing %s: %v", e.name, err)
	}
}

// Name returns the container name, for use in failure messages.
func (e *Envoy) Name() string { return e.name }

// AdminAddr returns the host address of the admin interface.
func (e *Envoy) AdminAddr() string { return e.hostPorts[AdminPort] }

// HostPort returns the host address for a published container port.
func (e *Envoy) HostPort(container uint32) string {
	addr, ok := e.hostPorts[container]
	if !ok {
		e.tb.Fatalf("envoytest: port %d was not published; add it to Config.Ports", container)
	}
	return addr
}

// Admin returns a client for this Envoy's admin interface.
func (e *Envoy) Admin() *admin.Client { return e.client }

// Logs returns everything the container has written so far.
func (e *Envoy) Logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	out, err := e.rt.runCombined(ctx, "logs", e.id)
	if err != nil && out == "" {
		return fmt.Sprintf("<could not read logs: %v>", err)
	}
	if out == "" {
		return "<no container output>"
	}
	return out
}

// ConfigDump fetches /config_dump, including endpoint data.
func (e *Envoy) ConfigDump() []byte {
	e.tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), admin.DefaultTimeout)
	defer cancel()
	raw, err := e.client.ConfigDump(ctx, true)
	if err != nil {
		e.tb.Fatalf("envoytest: fetch config dump from %s: %v", e.cfg.Name, err)
	}
	return raw
}

// Index fetches the config dump and parses it the way the server does.
func (e *Envoy) Index() *xds.Index {
	e.tb.Helper()
	ix, err := xds.Parse(e.ConfigDump())
	if err != nil {
		e.tb.Fatalf("envoytest: parse config dump from %s: %v", e.cfg.Name, err)
	}
	return ix
}

// Graph fetches, parses, and builds the pipeline graph in one step, which is
// what most scenarios actually want to assert on.
func (e *Envoy) Graph(opts graph.Options) *graph.Graph {
	e.tb.Helper()
	return graph.Build(e.Index(), opts)
}
