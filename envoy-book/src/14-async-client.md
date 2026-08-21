# 14. Requests Envoy Makes for Itself

*How does Envoy originate an HTTP request when there is no downstream stream to
hang it on, and why is it the same machinery anyway?*

Everything in Part II is driven from below. A connection arrives, a codec produces
headers, a filter chain runs, a router filter picks a cluster and a pool. Take the
downstream stream away and none of that starts. Yet Envoy constantly needs to send
a request nobody asked it for: to fetch its own configuration, authorise against an
external service, ship spans to a collector, copy traffic to a mirror cluster. That is what [`Http::AsyncClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/async_client.h#L69)
is for, and the reason it costs so little to explain is that underneath the seam it
is the router filter and the connection pools of [Chapter 8](./08-upstream.md),
unchanged.

## The interface

An `AsyncClient` is obtained from a cluster, not from the server:
[`ThreadLocalCluster::httpAsyncClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/thread_local_cluster.h#L159)
hands one back, and
[`ClusterEntry::httpAsyncClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/cluster_manager_impl.cc#L1494)
constructs it lazily on first use, so the object is per worker and per cluster and
lives on that worker's dispatcher. The destination is fixed before the first byte:
one client, one cluster.

There are three ways in. `send()` takes a complete `RequestMessagePtr` and reports
one buffered response; `startRequest()` returns an
[`OngoingRequest`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/async_client.h#L259) whose body and
trailers arrive later but which still delivers a single assembled response; and
`start()` returns a bare `Stream` with per-frame
[`StreamCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/async_client.h#L140). The
message-shaped `Callbacks` contract is only three methods — `onSuccess`,
`onFailure` and `onBeforeFinalizeUpstreamSpan` — and `onSuccess` fires for any
completed exchange, including a 503: at this layer a response is a success whatever
its status.

Everything else is configuration, carried by
[`StreamOptions`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/async_client.h#L280) (aliased as
`RequestOptions`), a builder-style struct that is the async client's substitute for
a route: a timeout, a retry policy (proto or parsed), `buffer_body_for_retry`, a
parent span with a child span name and a sampling override, a `ParentContext`
holding the originating `StreamInfo`, dynamic metadata for subset load balancing, a
filter state and buffer limits.

## What is synthetic and what is shared

[`AsyncStreamImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.h#L59) does
not build a filter chain. It *impersonates* one:

```cpp
class AsyncStreamImpl : public virtual AsyncClient::Stream,
                        public StreamDecoderFilterCallbacks,
                        public Event::DeferredDeletable,
                        public Logger::Loggable<Logger::Id::http>,
                        public LinkedObject<AsyncStreamImpl>,
                        public ScopeTrackedObject {
```

By implementing [`StreamDecoderFilterCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L615)
itself, it can own a real [`Router::ProdFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.h#L711)
and call `router_.setDecoderFilterCallbacks(*this)` at the end of its constructor.
[`sendHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.cc#L235)
then does little more than call `router_.decodeHeaders`,
and responses arrive through `encodeHeaders`/`encodeData`/`encodeTrailers`, which
forward to the caller's callbacks. Everything the router does from
there — cluster lookup, load balancing, the connection pools, retries, per-try
timeouts, outlier reporting — is the code [Chapter 8](./08-upstream.md) describes,
on the same pools serving downstream traffic.

Three things are fabricated. The route is a
[`NullRouteImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/null_route_impl.h#L234) built
per stream, carrying only the cluster name plus whatever the options supplied:
timeout, hash policy, retry policy from
[`createRetryPolicy`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.cc#L94), and
metadata match criteria borrowed from the parent request's route so subset load
balancing still works. The router `FilterConfig` is fabricated too — the
[`AsyncClientImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.h#L62)
constructor builds one with dynamic stats on, every other flag off, and the stat
prefix `http.async-client`, unless the caller overrides it with `setFilterConfig`.
And the `StreamInfo` is fresh, declared HTTP/1.1 regardless of what the upstream
negotiates.

Lifetime follows the usual rules: streams live in a list on the client,
[`cleanup`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.cc#L343) unlinks
and defers deletion (see [Chapter 11](./11-event-loop-and-threading.md)), and
destroying the client resets whatever is still in flight.
[`AsyncRequestSharedImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.h#L60)
adds response accumulation on top of the stream, capped by the runtime key
`http.async_response_buffer_limit`, which defaults to 32 MB, and finalises its child span in
[`onComplete`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_impl.cc#L414).

## gRPC on top

[`Grpc::AsyncClientImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/async_client_impl.h#L25)
is a framing layer, not a transport. The work is done by the per-call
`Grpc::AsyncStreamImpl` it creates:
[`initialize`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/async_client_impl.cc#L173)
looks up the configured cluster, calls `httpAsyncClient().start()`, synthesises the
`:path`, gRPC headers and configured initial metadata, and sends them; each
message is length-prefix framed onto the same stream, and `grpc-status` in trailers
becomes the completion status. Callers rarely touch it directly — the typed wrapper
[`Grpc::AsyncClient<Request, Response>`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/typed_async_client.h#L108)
serialises protos on the way in and parses them on the way out.

Which implementation you get depends on the `GrpcService` config.
[`factoryForGrpcService`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/async_client_manager_impl.cc#L136)
maps `envoy_grpc` to the above and `google_grpc` to
[`GoogleAsyncClientImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/google_async_client_impl.h#L174),
which is the upstream grpc++ library with its own sockets and a completion-queue
thread, compiled in only under `ENVOY_GOOGLE_GRPC`. The practical consequence is
that a `google_grpc` client sees none of the cluster's load balancing, circuit
breaking or transport socket configuration. Clients are shared:
[`getOrCreateRawAsyncClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/grpc/async_client_manager_impl.cc#L158)
caches them per worker, keyed by a hash of the config, with idle eviction.

## Request mirroring

Traffic mirroring is the case where an async request is spawned *by* a downstream
request, and its oddities all follow from the fact that it is not an
`UpstreamRequest`. Once the router knows which shadow policies apply, it copies the
headers and builds a `RequestOptions` setting the global timeout, a
child span named `mirror`, `is_shadow`, `discard_response_body`, the downstream
buffer account and limit, and the downstream `StreamInfo` as parent context — then
hands the copy to [`ShadowWriter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/shadow_writer.h#L18).

[`ShadowWriterImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/shadow_writer_impl.h#L16)
re-checks that the cluster still exists (CDS may have removed it) and appends a
`-shadow` suffix to the authority unless disabled, in both of its entry points:
`shadow` forwards a header-only request to the async client's `send()`, and
[`streamingShadow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/shadow_writer_impl.cc#L43)
forwards one with a body to `startRequest()`, handing the router back the
`OngoingRequest`. It is its own `AsyncClient::Callbacks`, and both `onSuccess`
and `onFailure` are empty bodies. That is the whole answer to why a mirrored
response can never influence retries, the response code or the downstream stream
(see [Chapter 8](./08-upstream.md)): nothing reads it.

Two links are deliberately left attached. The router keeps each streaming shadow
in a list and registers a destructor callback so it can drop the pointer when the
stream dies, and it registers a
[`StreamFilterSidestreamWatermarkCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/sidestream_watermark.h#L12),
which turns a shadow stream going over its buffer high watermark back into
`onDecoderFilterAboveWriteBufferHighWatermark` on the downstream filter chain — so a
slow mirror still applies backpressure, though never a result (see
[Chapter 12](./12-buffers-and-io.md)).

## Who else calls it

- **xDS.** Chapter 2 calls the control-plane connection an ordinary upstream
  connection; this is the layer that makes that true.
  [`GrpcStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/config_subscription/grpc/grpc_stream.h#L26)
  owns a `Grpc::AsyncClient` and its
  [`establishNewStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/config_subscription/grpc/grpc_stream.h#L61)
  is what opens the ADS or per-type stream that every mux in
  [Chapter 2](./02-configuration-and-xds.md) drives; reconnect backoff lives there
  too.
- **Callout filters.** `ext_authz` has both shapes:
  [`GrpcClientImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/common/ext_authz/ext_authz_grpc_impl.h#L42)
  over the typed gRPC client, and
  [`RawHttpClientImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/common/ext_authz/ext_authz_http_impl.h#L149)
  calling `httpAsyncClient().send()` directly. `ext_proc` is the same pattern with
  a long-lived bidirectional stream instead of a unary call.
- **Tracing exporters.** Zipkin's
  [`ReporterImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/tracers/zipkin/zipkin_tracer_impl.h#L134)
  batches spans and POSTs them with the async client, tracking in-flight handles in
  [`AsyncClientRequestTracker`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/async_client_utility.h#L11)
  so they are cancelled if the tracer dies first; OTLP uses
  [`OpenTelemetryGrpcTraceExporter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/tracers/opentelemetry/grpc_trace_exporter.h#L14)
  (see [Chapter 13](./13-observability-and-operations.md)).
- **Health reporting.** [`HdsDelegate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/health_discovery_service.h#L137)
  streams health responses to a management server through the gRPC client. Active
  health checking is the interesting counter-example:
  [`HttpHealthCheckerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/health_checkers/http/health_checker_impl.h#L48)
  does *not* use the async client, because a probe must target one specific host on
  its own connection rather than whatever the load balancer picks.

## What is different with no downstream

The absence shows up in three places. Flow control loses one direction:
`addDownstreamWatermarkCallbacks` on an async stream is an empty function, so the
router's usual attempt to read-disable the upstream when a downstream stalls has
nothing to act on. Instead the stream counts its own high-watermark calls and
offers them to an optional
[`SidestreamWatermarkCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/async_client.h#L32);
a caller that does have a downstream stream, like the shadow path, opts back in.

Buffering is explicit rather than inherited. `setBufferLimit` from a filter is an
`ENVOY_BUG` — the limit comes from the options or from a 64 KB default — and a body
is only retained for retries when `buffer_body_for_retry` is set, which is why the
gRPC layer forces it on for unary calls and leaves the choice to the caller — off by
default — for streams. Timeouts are the
route timeout the options supplied, plus the gRPC layer's one-second grace period
for a server half-close.

Finally, observability is opt-in. The stream gets its own `StreamInfo`, but with no
connection manager above it there is no access log by default; upstream logs are
emitted only when the caller passes a router `FilterConfig` that already has them
configured, which mirroring does. Stats still land, under `http.async-client`
rather than under a listener.
