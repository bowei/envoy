# How Envoy Works

This is a guide to the internals of the Envoy proxy: what the process looks like
when it is running, how configuration reaches the code that executes it, and what
actually happens to a request between the SYN that opens a connection and the last
byte of the response. It is written against the source rather than the
documentation. Every code reference in these fourteen chapters is a permalink into
the Envoy repository at a pinned commit, so a claim in the prose can always be
checked against the function it describes — and if the two disagree, the function
is right.

It is for people who already know roughly what Envoy does and now need to know how:
engineers writing extensions, operators debugging behaviour that the configuration
reference does not explain, and contributors trying to find the seam they are
supposed to cut along. It deliberately does not try to be reference material. There
is no extension-by-extension catalogue — extensions appear only when one of them
illustrates a mechanism. There are no configuration recipes; the chapters explain
what a field causes to happen, not which field to set. And the data plane API is
treated as an input, described only where its shape has left a mark on the
implementation. For all three, the official documentation at
[envoyproxy.io](https://www.envoyproxy.io/docs) is the right place to look.

## A note on the pinned commit

Every source link points at commit `981d3923fe`. The book therefore describes the
tree as of that commit, and the line numbers, file paths and symbol names it uses
are the ones valid there. Envoy moves quickly: expect names to have drifted if you
read this against a much later `main`. The structural claims age far better than
the identifiers.

## How to read this book

Part I is orientation. Read it once, in order, to get the process model, the
configuration pipeline and the extension mechanism into your head; almost every
later explanation assumes all three. Part II is the spine of the book and should be
read in order — it follows a single request from the accept loop to teardown, and
each chapter picks up where the last one stopped. Part III is reference-shaped
depth on the machinery that Part II leans on: read it on demand, when a chapter in
Part II hands you off to it, or straight through if you would rather understand the
foundations before the building.

## Table of contents

### Part I — The Map

1. [What Envoy Is: Process and Threading Model](./01-process-model.md) — the
   threads a running Envoy has, what each one owns, and why nothing is shared
   between them.
2. [Configuration and xDS](./02-configuration-and-xds.md) — how a protobuf on disk
   or on a gRPC stream becomes the objects that workers execute against.
3. [The Extension and Factory Framework](./03-extension-framework.md) — the
   registry, the factory contexts, and the build-time machinery that let every
   pluggable thing in Envoy arrive through the same door.

### Part II — The Journey of a Request

4. [The Accept Path: Listeners, Sockets and Transport](./04-accept-path.md) — from
   a bound socket to a connection with a transport socket and a filter chain.
5. [Network Filters and the HTTP Codecs](./05-network-filters-and-codecs.md) — the
   L4 filter chain, and how bytes become HTTP objects.
6. [The HTTP Connection Manager and the Filter Chain](./06-http-connection-manager.md)
   — the per-stream universe, the decoder and encoder chains, and local replies.
7. [Routing: From Headers to a Cluster](./07-routing.md) — how request headers are
   turned into one immutable `Route`, and everything that route carries.
8. [Upstream: Clusters, Load Balancing and Connection Pools](./08-upstream.md) —
   choosing a cluster, a host, and a connection, and the budgets that gate all
   three.
9. [The Response, Retries, Timeouts and Teardown](./09-response-and-teardown.md) —
   the response path and the mechanisms that can preempt it — retries, timeouts and
   internal redirect — then how a stream is recorded and destroyed.
10. [One Request, End to End](./10-one-request-end-to-end.md) — the whole of Part II
    as a single annotated call stack, with the points where it returns to the event
    loop marked.

### Part III — The Machine Room

11. [The Event Loop, Threading and Object Lifetime](./11-event-loop-and-threading.md)
    — the dispatcher, deferred deletion, and thread-local storage.
12. [Buffers, Flow Control and I/O](./12-buffers-and-io.md) — the slice-based
    buffer, the syscall layer, and how backpressure propagates from one socket to
    another.
13. [Stats, Logging, Tracing, Runtime and the Admin Interface](./13-observability-and-operations.md)
    — the machinery for finding out what happened and changing behaviour without a
    restart.
14. [Requests Envoy Makes for Itself](./14-async-client.md) — the async HTTP and
    gRPC clients that xDS, callout filters, tracers and request mirroring use when
    there is no downstream stream to hang a request on.
