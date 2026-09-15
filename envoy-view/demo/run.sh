#!/usr/bin/env bash
#
# Boot an Envoy in a container, point envoy-view at it, open the UI.
#
# Everything is torn down on Ctrl-C. Nothing is left behind except the built
# binary. See demo/bootstrap.yaml for what the demo config contains.
#
#   demo/run.sh              build, start both, open a browser
#   demo/run.sh --no-open    same, but do not open a browser
#   demo/run.sh --print      dump the graph to stdout and exit (no UI)
#
# Overridable: UI_PORT, ADMIN_PORT, HTTP_PORT, TCP_PORT, ENVOY_IMAGE,
# ENVOY_VERSION, ENVOY_TEST_RUNTIME.

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
demo="$repo/demo"

UI_PORT="${UI_PORT:-8080}"
ADMIN_PORT="${ADMIN_PORT:-9901}"
HTTP_PORT="${HTTP_PORT:-10000}"
TCP_PORT="${TCP_PORT:-10001}"

container="envoy-view-demo-$$"
open_browser=1
print_only=0

for arg in "$@"; do
	case "$arg" in
	--no-open) open_browser=0 ;;
	--print) print_only=1 open_browser=0 ;;
	-h | --help)
		sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^#\{1,2\} \{0,1\}//'
		exit 0
		;;
	*)
		echo "demo: unknown argument: $arg (try --help)" >&2
		exit 2
		;;
	esac
done

say() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() {
	printf '\033[1;31mdemo:\033[0m %s\n' "$*" >&2
	exit 1
}

# ---------------------------------------------------------------- the runtime

# Same search as internal/envoytest/runtime.go: a Homebrew or Docker Desktop
# install is routinely missing from a non-login shell's PATH, and "no runtime"
# on a machine that plainly has one is a bad first experience.
find_runtime() {
	local candidates=(docker podman nerdctl finch)
	[ -n "${ENVOY_TEST_RUNTIME:-}" ] && candidates=("$ENVOY_TEST_RUNTIME")

	local dirs=(/opt/homebrew/bin /usr/local/bin /Applications/Docker.app/Contents/Resources/bin)
	local name path dir
	for name in "${candidates[@]}"; do
		path="$(command -v "$name" 2>/dev/null || true)"
		if [ -z "$path" ]; then
			for dir in "${dirs[@]}"; do
				[ -x "$dir/$name" ] && path="$dir/$name" && break
			done
		fi
		[ -n "$path" ] || continue
		# Present is not the same as usable: the daemon or VM may be stopped.
		"$path" info >/dev/null 2>&1 || continue
		echo "$path"
		return 0
	done
	return 1
}

RT="$(find_runtime)" ||
	die "no working container runtime (tried docker, podman, nerdctl, finch).
     On macOS: 'colima start' or open Docker Desktop."

# The Envoy release is read from the test framework's pin rather than written
# out again here, so the demo and the tests can never disagree about it.
version="${ENVOY_VERSION:-$(sed -n 's/^const DefaultVersion = "\(.*\)"$/\1/p' "$repo/internal/envoytest/version.go")}"
[ -n "$version" ] || die "could not read DefaultVersion from internal/envoytest/version.go"
image="${ENVOY_IMAGE:-envoyproxy/envoy:$version}"

# ------------------------------------------------------------------- teardown

view_pid=""
cleanup() {
	trap - EXIT INT TERM
	echo
	say "stopping"
	[ -n "$view_pid" ] && kill "$view_pid" 2>/dev/null || true
	"$RT" rm -f "$container" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# --------------------------------------------------------------------- build

say "building envoy-view"
(cd "$repo" && go build -o envoy-view ./cmd/envoy-view)

# ---------------------------------------------------------------- start Envoy

for port in "$ADMIN_PORT" "$HTTP_PORT" "$TCP_PORT" "$UI_PORT"; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		die "port $port is already in use. Set UI_PORT/ADMIN_PORT/HTTP_PORT/TCP_PORT to move it."
	fi
done

say "starting Envoy $version ($(basename "$RT"))"
"$RT" create \
	--name "$container" \
	--label envoy-view-demo=1 \
	--publish "127.0.0.1:$ADMIN_PORT:9901" \
	--publish "127.0.0.1:$HTTP_PORT:10000" \
	--publish "127.0.0.1:$TCP_PORT:10001" \
	"$image" \
	envoy -c /etc/envoy/bootstrap.yaml --log-level info >/dev/null

# Copied in rather than bind-mounted: a bind mount has to survive the runtime's
# VM sharing rules and the image's unprivileged "envoy" user, and copying has
# neither problem.
for f in bootstrap.yaml rds.yaml eds.yaml; do
	"$RT" cp "$demo/$f" "$container:/etc/envoy/$f"
done

"$RT" start "$container" >/dev/null

say "waiting for Envoy to report itself live"
for _ in $(seq 1 300); do
	if [ "$(curl -fsS "http://127.0.0.1:$ADMIN_PORT/ready" 2>/dev/null || true)" = "LIVE" ]; then
		ready=1
		break
	fi
	sleep 0.2
done
[ "${ready:-}" = 1 ] || {
	"$RT" logs "$container" 2>&1 | tail -30
	die "Envoy never became live (logs above)"
}

# Send a little traffic so the counters and access log are not empty. The
# config dump does not depend on this; it just makes the demo feel alive.
curl -fsS "http://127.0.0.1:$HTTP_PORT/api/items" >/dev/null 2>&1 || true
curl -fsS "http://127.0.0.1:$HTTP_PORT/healthz" >/dev/null 2>&1 || true

# ------------------------------------------------------------ the -print path

if [ "$print_only" = 1 ]; then
	say "graph"
	"$repo/envoy-view" -envoy-admin "127.0.0.1:$ADMIN_PORT" -print graph
	exit 0
fi

# ----------------------------------------------------------- start envoy-view

say "starting envoy-view"
"$repo/envoy-view" -envoy-admin "127.0.0.1:$ADMIN_PORT" -addr "127.0.0.1:$UI_PORT" &
view_pid=$!

for _ in $(seq 1 100); do
	curl -fsS -o /dev/null "http://127.0.0.1:$UI_PORT/" 2>/dev/null && break
	kill -0 "$view_pid" 2>/dev/null || die "envoy-view exited during startup"
	sleep 0.1
done

url="http://127.0.0.1:$UI_PORT"
cat <<EOF

  envoy-view    $url
  Envoy admin   http://127.0.0.1:$ADMIN_PORT
  ingress       http://127.0.0.1:$HTTP_PORT   (tcp proxy on $TCP_PORT)

  Things to try

    curl localhost:$HTTP_PORT/api/items     80/20 across service_a and service_canary
    curl localhost:$HTTP_PORT/healthz       answered by Envoy, no cluster behind it
    curl -i localhost:$HTTP_PORT/old        a redirect route
    curl -i localhost:$HTTP_PORT/legacy     503: the route points at a cluster that does not exist

  In the UI, /legacy is the red node, and the problems panel at the bottom
  jumps to it. The "catalog" cluster reads 2/3 healthy because eds.yaml
  delivers one endpoint as UNHEALTHY.

  Edit demo/rds.yaml (bump version_info) and hit Refresh to watch the graph
  pick up a live RDS update.

  Ctrl-C to stop both.

EOF

if [ "$open_browser" = 1 ]; then
	if command -v open >/dev/null 2>&1; then
		open "$url"
	elif command -v xdg-open >/dev/null 2>&1; then
		xdg-open "$url" >/dev/null 2>&1 || true
	fi
fi

wait "$view_pid"
