# Testing plan

## What we are actually testing

envoy-view has one job: read an Envoy config dump and tell the truth about it.
Almost every way it can fail is a way of misreading real Envoy output — a
section shaped differently than we assumed, a field Envoy leaves empty, a
lifecycle state we never saw.

That makes hand-written fixtures the central risk. Until `internal/envoytest`
existed, every fixture under `internal/xds/testdata` was written from the
ConfigDump proto definitions, which means the tests asserted what we *believed*
Envoy emits. Belief and assertion came from the same place, so a wrong belief
passed. Three were wrong, and the first container run found all three:

- `ScopedRoutesConfigDump` is emitted as `{}` on every dump, so a warning meant
  for unusual configs fired on all of them.
- `version_info` is per resource, not per response — an unchanged resource keeps
  the version it last changed at.
- A rejected update reaches the dump only for listeners; CDS leaves
  `error_state` unset and a proto-validation failure is recorded nowhere.

So the organising principle is: **anything asserted about Envoy's output must
ultimately be traceable to output a real Envoy produced.**

## Layers

| Layer | Runs | Cost | Needs | Answers |
| --- | --- | --- | --- | --- |
| 1. Unit | `make test-short` | ms | nothing | Is the parser/builder self-consistent? |
| 2. Scenario | `make test-container` | seconds | a container runtime | Does real Envoy agree? |
| 3. Fixture capture | `make fixtures` | seconds | a container runtime | Are the unit fixtures still real? |
| 4. Version matrix | CI, nightly | minutes | runtime + network | Did a new Envoy change the contract? |
| 5. Property/corpus | `make test-short` | ms | nothing | Does anything crash or lie on hostile input? |
| 6. UI | `npm test` + captured dumps | ms | node | Does the view match the graph? |

Layers 1 and 5 must stay hermetic and fast — they are the inner loop and they
must work on a laptop with no Docker. Layers 2–4 are where truth enters the
system. Layer 3 is the bridge: scenarios generate the fixtures that the
hermetic layers consume, so speed and truth stop being a trade-off.

## The configuration matrix

Two axes generate most of the space worth covering.

**Resource kind × delivery mechanism.** Delivery decides which dump section a
resource lands in and which fields are populated, and that is precisely what
the parser keys off.

| | bootstrap (static) | filesystem xDS | gRPC ADS |
| --- | --- | --- | --- |
| Listener | ✅ | ✅ | phase 2 |
| Cluster | ✅ | ✅ | phase 2 |
| RouteConfiguration | ✅ inline in HCM | ✅ RDS | phase 2 |
| ClusterLoadAssignment | ✅ | ✅ EDS | phase 2 |
| Secret | ✅ | ✅ SDS | phase 2 |
| Scoped routes | ❌ not modelled | ❌ | ❌ |
| ECDS filter configs | ❌ not modelled | ❌ | ❌ |

**Resource state × how it is reached.**

| State | How to produce it | Covered by |
| --- | --- | --- |
| `static` | bootstrap `static_resources` | smoke scenario |
| `active` | any xDS delivery that succeeds | dynamic scenarios |
| `warming` | listener whose RDS never arrives | lifecycle scenarios |
| `draining` | listener removed via LDS, within the drain window | lifecycle scenarios (may prove impractical to catch reliably) |
| active + warming together | push an update that cannot initialise behind a serving one | lifecycle scenarios |
| rejected | update that fails at application time | dynamic scenarios |

**Failure modes**, which are the reason the tool exists — a healthy config is
readable without it:

- Dangling reference: route → missing cluster, transport socket → missing SDS
  secret, HCM → missing RDS config.
- Rejected update, with the serving config older than the pushed one.
- Resource that will not decode, or carries an extension type this binary was
  not compiled against.
- Cluster with no endpoints, or no healthy endpoints.
- Duplicate or shadowed names across lifecycle sections.

## Phase 1 — done

`internal/envoytest`: runtime discovery, a pinned Envoy image, scenario
lifecycle, filesystem xDS with atomic updates, and `WaitFor` polling so
eventual consistency does not become flakiness. Bootstraps are validated
against `envoy.config.bootstrap.v3.Bootstrap` plus protoc-gen-validate before a
container starts, so a misspelled field fails in the test process with the
field named, rather than as a container exiting with status 1.

## Phase 2 — a gRPC ADS control plane

Filesystem xDS reaches most dynamic states cheaply, but not all of them, and it
diverges from production in ways that matter:

- `error_state.failed_version_info` stays empty; a gRPC control plane sets it.
- There is no NACK *round trip*, so we never observe what Envoy sends back.
- Delta xDS, resource TTLs, `client_status`, and per-subscription state have no
  filesystem equivalent.
- Real deployments use ADS, so ADS-specific ordering bugs are invisible to us.

The cost is a `github.com/envoyproxy/go-control-plane` (server) plus gRPC
dependency — currently only the `envoy` protos submodule is vendored — and
container→host networking (`host.docker.internal`, with
`--add-host=host.docker.internal:host-gateway` on Linux and rootless podman).

Recommendation: do it, but keep it behind a build tag so the default `go test
./...` dependency graph stays small.

## Phase 3 — fixture capture and drift detection

`make fixtures` regenerates `internal/xds/testdata/*.json` from the scenarios.
CI runs the same capture and fails if the working tree changes, so a fixture
can never quietly drift from what Envoy emits. Unit tests keep running against
files, offline and in milliseconds; those files are now transcripts rather than
guesses.

Golden graph output belongs here too: render `graph.Build` through the existing
text formatter in `internal/graph/text.go` and diff against a checked-in
golden. That catches builder regressions that node-count assertions miss, and
the diff is readable in review.

## Phase 4 — the version matrix

The pin is `DefaultVersion` in `internal/envoytest/version.go`, deliberately
fixed so a test baseline cannot move on its own. To keep the pin honest:

- `ENVOY_TEST_ONLINE=1` enables a test comparing the pin against the newest
  GitHub release, so an upgrade is a visible commit rather than a silent drift.
- Nightly CI runs the scenario suite across `ENVOY_VERSION` values covering the
  last few supported minor releases. Envoy supports roughly four at a time.

This is what turns "our parser broke" and "Envoy changed" into distinguishable
events, which is the entire reason for pinning rather than tracking latest.

## Phase 5 — property and corpus testing

Cheap, hermetic, and good at finding what scenarios miss because nobody thought
to write them:

- **Fuzz `xds.Parse`** with `go test -fuzz`, seeded from every captured fixture.
  It must never panic; it may return an error. The parser handles attacker-ish
  input the moment someone uploads a dump through the UI, which envoy-view
  supports.
- **Graph invariants as properties**, checked on every fixture and every
  scenario: node IDs unique; every edge endpoint resolves to a real node; every
  problem names a node in the graph; every node reachable from a root when a
  root filter is applied. These are the invariants the React Flow UI silently
  depends on.
- **Round-trip**: every resource in the index must retain `Raw` byte-for-byte,
  because that is what the detail pane shows. A decode failure must never lose
  the raw JSON.

## Phase 6 — UI

The React side currently has unit tests only for the fuzzy matcher. The useful
additions, in order of value per effort:

1. Layout invariants on captured graphs (no overlapping nodes, finite
   positions, expected rank count) — already done once as a throwaway script;
   it should be a committed test.
2. Component tests for `DetailPane` and `ProblemsPanel` against captured graph
   JSON, so the contract between Go and TypeScript types is exercised.
3. A single end-to-end smoke test: serve a captured dump, load the page, assert
   the expected node labels render. The headless-Chrome screenshot loop used
   during development is the obvious basis.

## Explicitly out of scope

- **Testing Envoy itself.** If Envoy rejects a config, that is a scenario
  authoring error, not a finding — except where we assert that Envoy rejects it.
- **Traffic correctness.** envoy-view never sees requests. Scenarios publish
  ports only when a test needs a listener to reach a serving state.
- **Scoped routes and ECDS** until the graph models them. Testing a view that
  does not exist would only pin down the absence.

## Running it

```sh
make test-short      # everything hermetic; no container runtime needed
make test-container  # the scenarios, against a real Envoy
make fixtures        # regenerate internal/xds/testdata from Envoy (phase 3)
```

Environment: `ENVOY_VERSION`, `ENVOY_IMAGE`, `ENVOY_TEST_RUNTIME`,
`ENVOY_TEST_REQUIRE`, `ENVOY_TEST_KEEP`, `ENVOY_TEST_ONLINE` — see
`internal/envoytest/doc.go`.

In CI, set `ENVOY_TEST_REQUIRE=1`. Without it a broken runtime makes every
scenario skip, and the suite passes green having tested nothing.
