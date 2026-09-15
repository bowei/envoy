BIN := envoy-view
UI_SRC := $(shell find web/src web/index.html -type f 2>/dev/null)
UI_OUT := internal/webui/dist/index.html

.PHONY: all build demo test test-go test-ui test-container test-short vet fmt ui ui-dev clean

all: build

# The UI is committed under internal/webui/dist, so a plain build needs no npm.
build:
	go build -o $(BIN) ./cmd/envoy-view

# Boot a demo Envoy in a container, point envoy-view at it, open the UI.
# Needs a container runtime; Ctrl-C tears both down.
demo:
	./demo/run.sh

test: test-go test-ui

test-go:
	go test ./...

# Skipped rather than failed when the UI dependencies are not installed, so
# "make test" works in a checkout that has never run npm.
test-ui:
	@if [ -d web/node_modules ]; then \
		cd web && npm test --silent && npm run --silent typecheck; \
	else \
		echo "skipping UI tests: run 'make ui' first"; \
	fi

# Everything except the container scenarios, for a quick inner loop or a
# machine with no container runtime.
test-short:
	go test -short ./...

# The scenarios that boot a real Envoy. ENVOY_TEST_REQUIRE turns "no runtime"
# into a failure, so CI cannot pass by silently skipping all of them.
test-container:
	ENVOY_TEST_REQUIRE=1 go test -v -count=1 -timeout 15m ./internal/envoytest/

vet:
	go vet ./...

fmt:
	go fmt ./...

# Rebuild the embedded web UI. Vite is configured to write straight into
# internal/webui/dist, which is committed so "go install" works without npm.
ui: $(UI_OUT)

$(UI_OUT): $(UI_SRC) web/package.json
	cd web && npm install && npm run build

# Vite dev server with hot reload, proxying /api to a locally running
# envoy-view. Run "make build && ./envoy-view" in another terminal.
ui-dev:
	cd web && npm install && npm run dev

clean:
	rm -f $(BIN)
	rm -rf web/node_modules
