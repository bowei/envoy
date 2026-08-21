# 6. The HTTP Connection Manager and the Filter Chain

*What is the per-stream object that an HTTP request lives inside, and how does it
walk a chain of filters in each direction?*

Everything Envoy does to an HTTP request happens inside one network filter. The HTTP connection
manager — universally abbreviated HCM — sits at the end of a listener's network filter chain,
owns a protocol codec (see [Chapter 5](./05-network-filters-and-codecs.md)), and turns a
connection's byte stream into a sequence of independent
streams. Each stream walks a chain of HTTP filters, the last of which is normally the router
(see [Chapter 8](./08-upstream.md)). HTTP/1.1, HTTP/2 and HTTP/3 differ enormously at the wire
level and almost not
at all above it; the codec absorbs that difference, so routing, authorization and logging are
written once, against a protocol-agnostic stream API.

## A network filter that speaks HTTP

[`ConnectionManagerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.h#L60)
implements `Network::ReadFilter`, `ServerConnectionCallbacks` and
`Network::ConnectionCallbacks` at once. Bytes arrive at
[`onData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L515), which
creates the codec lazily on first data — HTTP/3 codecs are created in `onNewConnection`
instead, because QUIC has already demultiplexed — and hands the buffer to `codec_->dispatch`.
`onData` always returns `StopIteration`: the HCM is terminal, and no network filter after it
sees the data.

When the codec recognises the start of a request it calls back into
[`newStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L410),
which allocates an
[`ActiveStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.h#L145) and links it into the
manager's `streams_` list. An `ActiveStream` is the per-request universe: the `RequestDecoder`
the codec pushes frames into, the `FilterManagerCallbacks` the filter chain pushes the response
back through, a `ScopeTrackedObject` so a crash dump can name the request, and the owner of its
timers, span, headers and route cache. It holds a
[`DownstreamFilterManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.h#L1221) by
value, declared after the header maps because filters may still read those headers while being
destroyed.

For HTTP/1 the codec pauses after a complete message, so `onData` only redispatches when
`streams_` is empty; back-pressure on a pipelined connection falls out of that. Under HTTP/2
and HTTP/3 many `ActiveStream`s coexist on one connection.

## The per-stream record

Every stream also carries a record of what happened to it.
[`StreamInfo`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L736) is the interface and
[`StreamInfoImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/stream_info_impl.h#L142) the
implementation; the `DownstreamFilterManager` holds one by value, built from the stream's
protocol, the connection's address provider and the time source. Anything that later needs to
describe the request reads it: the start time and the
[`DownstreamTiming`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L385) marks, bytes received
and sent, the response code, and the
[`UpstreamInfo`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L574) naming the host that was
actually dialled.

Two of its fields are written on nearly every error path in the rest of this chapter. The
response-code-details string is usually one of the constants in
[`ResponseCodeDetailValues`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L146) —
`missing_host_header`, `path_normalization_failed` — and says precisely which check rejected the
request. The response flags are values of
[`CoreResponseFlag`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L40): `NoRouteFound`,
`NoHealthyUpstream`, `UpstreamRequestTimeout`, `StreamIdleTimeout`,
`DownstreamConnectionTermination` and two dozen more, abbreviated to a few letters each by
[`ResponseFlagUtils`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/utility.h#L50) for the
`%RESPONSE_FLAGS%` access-log field. What the access log does with all of this is
[Chapter 9](./09-response-and-teardown.md).

## Before the filters run

[`ActiveStream::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1352)
carries a long comment explaining that its ordering is "complicated, but important": Envoy
wants to do as little as possible before creating the filter chain, so that as many requests as
possible get custom filter behaviour, access logging above all; but it cannot route a request
whose headers are nonsense. So the method is a sequence of cheap rejections — missing `Host`,
non-relative `:path`, headers failing `HeaderUtility::requestHeadersValid` — each calling
`sendLocalReply` with its own response-code-details string. Under load-shed it calls
`skipFilterChainCreation()`, so a 503 costs no filters at all; the point it probes,
`envoy.load_shed_points.http_connection_manager_decode_headers`, is one of the named set
in [Chapter 1](./01-process-model.md).

Then come the mutations, all in `ConnectionManagerUtility`.
[`maybeNormalizePath`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L770)
strips URI fragments and applies the escaped-slash policy, returning `Continue`, `Reject` or
`Redirect` — and a `Redirect` for a gRPC request becomes a rejection, since gRPC clients do not
follow redirects.
[`mutateRequestHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L121)
does the security-relevant work, and only on the first pass — an internally recreated stream
skips it. It drops hop-by-hop headers, sanitizes `TE`, and decides who the client is: with
`use_remote_address`, Envoy reads the trusted address out of `x-forwarded-for`,
`xff_num_trusted_hops` entries from the right, and only then appends the direct peer to it
([`appendXff`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L111));
otherwise the configured original-IP-detection extensions decide, and may demand rejection. That address
determines whether the request counts as *internal*, which determines whether
[`cleanInternalHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L351)
strips the `x-envoy-*` control headers, such as `x-envoy-retry-on`, that an external client
must never be allowed to set. Finally `x-request-id` is set through the request-ID extension,
and
[`mutateTracingRequestHeader`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L390)
turns that ID, modulo 10000, into a sampling decision (see
[Chapter 13](./13-observability-and-operations.md)).

That sequence is what a default build does. Envoy also has a compile flag, `ENVOY_ENABLE_UHV`,
that moves header validation out of the connection manager and the codecs and into an extension
implementing [`ServerHeaderValidator`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/header_validator.h#L81),
created per stream by
[`makeHeaderValidator`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_config.h#L571) and
called from [`validateHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1228)
before routing rather than after the sanity checks. In such a build the `maybeNormalizePath`
block above is compiled out — it sits inside `#ifndef ENVOY_ENABLE_UHV` — and normalization
becomes
[`PathNormalizer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/http/header_validators/envoy_default/path_normalizer.h#L14)
inside the extension, with the per-protocol character and pseudo-header rules in
[`Http1HeaderValidator`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/http/header_validators/envoy_default/http1_header_validator.h#L11)
and its HTTP/2 sibling. The `Host` and relative-path checks are outside the guard and stay put.
Grep for the guard before trusting this file.

## Building the chain, once per stream

Only after the route has been resolved (see [Chapter 7](./07-routing.md)) does the stream call
[`createDownstreamFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1153),
which delegates to `FilterManager::createFilterChain`. If the request carries `Upgrade`, or is
a CONNECT,
[`createUpgradeFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1773)
is tried first and may build a different chain, or reject the upgrade, which the HCM turns into
a 403.

The factory adds filters implementing
[`StreamDecoderFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L1008),
[`StreamEncoderFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L1212), or both. A filter never
talks to the `FilterManager` directly, only to its own wrapper through
[`StreamFilterCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L429): one registered as a
`StreamFilter` is wrapped twice, once as an
[`ActiveStreamDecoderFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.h#L247)
and once as an
[`ActiveStreamEncoderFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.h#L340),
each setting itself as the filter's callbacks object. A `Cleanup` fixes every wrapper's `entry_`
iterator once the vectors are final; the chain is immutable afterwards, which makes those
iterators safe to hold.

Per-stream construction is also where per-route configuration takes effect.
[`createFilterChainForFactories`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.cc#L18)
asks `callbacks.filterDisabled(name)` for each configured filter and skips the ones the route
disables. Configuration that merely *tunes* a filter is fetched later, by name, via
[`mostSpecificPerFilterConfig`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L325)
or `perFilterConfigs`, which consult the
[`PerFilterConfigs`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/per_filter_config.h#L10) of both the
route and its virtual host, the route's entry taking precedence; those objects are built at
config load time from the typed configs, honouring their `disabled` and `is_optional` flags.
Because a filter may cache one while decoding and use it while encoding, `ActiveStream` keeps
every route it has cached alive.

## Iterating the decoder chain

[`FilterManager::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L592)
is a plain loop over `decoder_filters_`. Each iteration sets a bit in `filter_call_state_`, so
that callbacks made *from inside* a filter can tell where they are, computes that filter's
private `end_stream_`, calls the filter, and interprets the returned
[`FilterHeadersStatus`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L37) in
[`commonHandleAfterHeadersCallback`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L165):

- `Continue` — carry on to the next filter.
- `StopIteration` — stop headers here; data and trailers still reach this filter.
- `ContinueAndDontEndStream` — keep iterating but hide `end_stream`, for a filter that will
  inject a body later.
- `StopAllIterationAndBuffer` — stop headers, data and trailers, buffering the body.
- `StopAllIterationAndWatermark` — the same, but push back via watermarks (see
  [Chapter 12](./12-buffers-and-io.md)).

The distinction between "stop" and "stop all" is recorded as an `IterationState` on the
wrapper, and it decides where iteration resumes:
[`commonDecodePrefix`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1000)
normally restarts at `std::next(filter->entry())`, but a filter that stopped *all* frame types
never saw its own data callback, so `iterate_from_current_filter_` sends the next frame back
into it.

Resumption is
[`commonContinue`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L61),
reached from `continueDecoding()` or `continueEncoding()`. It replays whatever the filter
missed, in order — 1xx headers, headers, metadata, buffered body, trailers — re-checking
`canContinue()` between steps, since any of them may have produced a local reply that makes
further iteration illegal.

Buffering is deliberately minimal: one request buffer, `buffered_request_data_`, shared by all
decoder filters.
[`commonHandleBufferData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L199)
refuses to re-buffer data already in it:

```cpp
  if (bufferedData().get() != &provided_data) {
    if (!bufferedData()) {
      bufferedData() = createBuffer();
    }
    bufferedData()->move(provided_data);
  }
```

A filter that injects body data calls
[`addDecodedData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L832),
which switches on `filter_call_state_`: inside `decodeHeaders` or `decodeData` the data joins
that buffer; inside `decodeTrailers` it is dispatched inline to later filters; anywhere else it
is a bug that yields a 502. The buffer is a watermark buffer bounded by the connection
manager's limit, and overrunning it calls
[`requestDataTooLarge`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L569),
which raises a watermark if the filters are streaming and otherwise sends a 413.

## The encode path and its asymmetries

The encode path is the mirror image, and the mirroring is literal: the encoder wrappers live in
a vector in configuration order, but the container hands out reverse iterators.

```cpp
struct StreamEncoderFilters {
  using Element = ActiveStreamEncoderFilter;
  using Iterator = std::vector<ActiveStreamEncoderFilterPtr>::reverse_iterator;

  Iterator begin() { return entries_.rbegin(); }
  Iterator end() { return entries_.rend(); }
```

Status handling, buffering and `commonContinue` are shared code; the asymmetries lie elsewhere.
The decoder chain has a terminal filter — the router, which
[`isTerminalDecoderFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L2090)
identifies as simply the last entry — while the encoder chain's sink is the codec, reached
through `FilterManagerCallbacks::encodeHeaders`. The encode path has an extra frame type, 1xx
informational headers, with its own two-valued
[`Filter1xxHeadersStatus`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L114). And
[`commonEncodePrefix`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L966)
does bookkeeping its decoding twin does not: when a response arrives with `end_stream` it
latches `observed_encode_end_stream_` and, unless independent half-close is enabled, aborts the
*decoder* chain, since nobody wants the rest of the request.

After the last encoder filter,
[`FilterManager::encodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1314)
runs `checkRequiredResponseHeaders` — a filter that deleted `:status` gets a 502 rather than a
malformed response — and only then calls
[`ActiveStream::encodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1906),
which adds `date` and `server`, applies
[`mutateResponseHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_utility.cc#L688),
evaluates drain and keep-alive policy, and writes to the codec.

## Passing information between filters

Filters never call each other. A filter that decides something — this caller is authorized,
this is the tenant it belongs to — and wants a later filter or the access log to know it uses
one of two side channels, both hanging off the `StreamInfo` introduced above.

The first is [`FilterState`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/filter_state.h#L53), a name-to-object
map of arbitrary C++ types reached through `streamInfo().filterState()`. Values derive from
`FilterState::Object` and come back out of `getDataReadOnly<T>`, which is a `dynamic_cast`, so
producer and consumer must agree on the concrete class; an extension that cannot link it
registers an [`ObjectFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/filter_state.h#L123) under the
object's name in the `filter_state.object` category and builds the object from bytes instead.
Storage is not flat. Every entry has a life span:

```cpp
  enum LifeSpan { FilterChain, Request, Connection, TopSpan = Connection };
```

[`FilterStateImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/filter_state_impl.h#L15) is
therefore a chain of maps, one per span, linked by parent pointers that
[`maybeCreateParent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/filter_state_impl.cc#L8)
fills in lazily. The distinction matters: a filter chain is rebuilt on an internal redirect, so
`Request` outlives `FilterChain`, and `Connection` state is visible to every stream multiplexed
onto the connection — which is why the HCM hands the connection's filter state to the
`DownstreamFilterManager` as its ancestor. Setting one name at two life spans is an
`ENVOY_BUG`. An object may also be marked
[`StreamSharingMayImpactPooling`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/filter_state.h#L28),
which copies it by reference into the upstream connection's filter state; if the object also
implements `Hashable`, its hash is folded into the transport socket options by
[`hashKey`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/transport_socket_options_impl.cc#L22) and so into the
connection pool key, and streams with distinct hashes cannot share an upstream connection (see
[Chapter 8](./08-upstream.md)).

The second channel is dynamic metadata: an `envoy::config::core::v3::Metadata` proto keyed by
filter name, written with
[`setDynamicMetadata`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L981). It is weaker
than filter state — protobuf `Struct` values only — and that is its point: being a proto it
serialises straight into a structured access log with no registration at all. It is also what
the load balancer reads. The router's implementation of
[`metadataMatchCriteria`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/load_balancer.h#L95) takes the
route's configured criteria and merges the
[`ENVOY_LB`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/well_known_names.h#L34) — `envoy.lb` — section of
first the connection's and then the request's dynamic metadata over the top, request winning;
the [result](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.h#L365) is cached, since the route
cannot change afterwards. That merged set is what subset load balancing matches hosts against
(see [Chapter 8](./08-upstream.md)).

## Local replies, and the end of the stream

A local reply is any response Envoy generates itself. It is the subtlest control flow here,
because it can be raised from anywhere: a filter, a timeout, a buffer overrun, header
validation.
[`sendLocalReply`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1026)
first stops whichever chain is running, then gives every filter a veto through
[`onLocalReply`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1014);
a filter returning `LocalErrorStatus::ContinueAndResetStream` converts the reply into a stream
reset. After that there are three cases, chosen by how far the response has already got:

- No response headers yet: run the reply through the encoder chain, so compression, logging and
  header-mutation filters see it. If no chain exists, one is created first, so access logging
  still works.
- Response headers exist but nothing non-1xx has reached the codec: bypass the filters with
  [`sendDirectLocalReply`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1193),
  since re-entering filters that are mid-response would corrupt their state machines.
- Headers already on the wire: nothing can be said, so reset the stream.

Even the first case has a re-entrancy problem: the reply is usually raised from inside a
decoder filter's callback, and encoding there would run the encoder chain underneath the
decoder chain's stack frame. So when `filter_call_state_` shows a decode in progress the reply
is only *prepared*, by
[`prepareLocalReplyViaFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1105),
and the decode loop runs it via
[`executeLocalReplyIfPrepared`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1144)
once the current filter returns. Body and status come from the configured
[`LocalReply`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/local_reply/local_reply.h#L13) object, which formats
the body with the first matching
[`ResponseMapper`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/local_reply/local_reply.cc#L65) — an access-log
filter plus optional status, body and header rewrites — or with the default formatter.

When encoding finishes,
[`maybeEndEncode`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1631)
calls `endStream()` on the callbacks, which lands in
[`doEndStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L269);
with independent half-close enabled it instead defers to
[`checkAndCloseStreamIfFullyClosed`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1659),
which waits for both directions. What happens after that — the access log, the
`onDestroy()` sweep over the filters, and why the `ActiveStream` is deferred-deleted rather
than freed inline — is the teardown section of
[Chapter 9](./09-response-and-teardown.md).
