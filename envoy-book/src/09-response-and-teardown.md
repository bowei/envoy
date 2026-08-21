# 9. The Response, Retries, Timeouts and Teardown

*What comes back, what can preempt it — a retry, a timeout, a reset — and how does
a finished stream get recorded and destroyed?*

A request that reaches an upstream host is only half a round trip. The other half is
harder, because the response path is where every failure mode surfaces at once: the
upstream may answer, or reset, or answer slowly, or answer with a 503 that Envoy is
supposed to hide by trying somewhere else. The router filter decides, for each of those
outcomes, whether the downstream client learns about it. This chapter follows the bytes
back from the upstream codec, examines the mechanisms that can preempt them — retries,
internal redirects, timeouts and resets — and finishes with how a stream is recorded and
destroyed.

## Back up the chain

Response data enters through the upstream codec and lands in
[`UpstreamCodecFilter::CodecBridge::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_codec_filter.cc#L149),
which stamps `onFirstUpstreamRxByteReceived` and pushes the headers into the *upstream*
filter chain's encode direction. That chain's terminal callback object is
[`UpstreamRequestFilterManagerCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.h#L272),
whose `encodeHeaders` simply calls
[`UpstreamRequest::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L295).
The naming inverts because `UpstreamRequest` implements `Http::ResponseDecoder` through the
legacy [`UpstreamToDownstream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/router.h#L1560) interface: what
the upstream chain encodes, the upstream request decodes.

`UpstreamRequest::decodeHeaders` rearms the per-try idle timer, drops unsupported 1xx
responses on the floor, and hands off to
[`Filter::onUpstreamHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1843).
That function is the decision point of the whole response path. In order it feeds the
outlier detector, asks the retry state whether these headers are retriable, considers an
internal redirect (below), adds `x-envoy-upstream-service-time`, records the response code, and —
critically — calls
[`Filter::resetOtherUpstreams`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1822)
to kill any sibling attempts before forwarding. Only then does it call
`callbacks_->encodeHeaders`, crossing from the router into the downstream encoder chain.

Body and trailers are simpler:
[`Filter::onUpstreamData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2046) and
[`Filter::onUpstreamTrailers`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2070)
both assert that exactly one upstream request remains, settle gRPC success/error
accounting, and forward. When end-of-stream arrives,
[`Filter::onUpstreamComplete`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2105)
records response timing, defers deletion of the `UpstreamRequest`, and calls
[`Filter::cleanup`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1250), which destroys the
retry state and disables the global timeout.

`callbacks_->encodeHeaders` is the boundary of this chapter's concern: from there the response
walks the downstream encoder chain and reaches the codec, which is
[Chapter 6](./06-http-connection-manager.md)'s territory. It comes back here at the end, when
[`FilterManager::maybeEndEncode`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1631)
decides the response is complete and teardown begins.

## Retries

Retries are not a wrapper around the request; they are a state machine owned by the router
filter for the life of the stream.
[`RetryStateImpl::create`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L29)
allocates one only if the route or the `x-envoy-retry-on` headers ask for retries — or if
the cluster speaks HTTP/3 and the request uses a safe method, in which case Envoy silently
adds a retry on 425 so that a rejected 0-RTT request can be replayed. Either way — allocated
or not — `create` then strips the `x-envoy-retry-*`, `x-envoy-max-retries`,
`x-envoy-hedge-on-per-try-timeout` and `x-envoy-upstream-rq-per-try-timeout-ms` request
headers so they never reach the upstream.

The policy itself is a bitset.
[`RetryStateImpl::parseRetryOn`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L179)
turns `5xx`, `gateway-error`, `connect-failure`, `reset`, `reset-before-request`,
`retriable-status-codes` and the rest into flags, and a sibling parses the gRPC variants.
Two predicates then read that bitset:
[`wouldRetryFromHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L351)
for a response that arrived, and
[`wouldRetryFromReset`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L431)
for one that did not. Both return a `RetryDecision` of `RetryImmediately`,
`RetryWithBackoff` or `NoRetry` — the immediate variant exists mainly for HTTP/3 fallback,
where the failure is known to be a handshake problem and waiting buys nothing.

A decision to retry is not permission to retry. That gate is
[`RetryStateImpl::shouldRetry`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L269),
which decrements the remaining-tries counter, then asks the cluster's resource manager
whether another concurrent retry may be created, then checks the `upstream.use_retry`
runtime key. Its answer is one of five values:

```cpp
enum class RetryStatus {
  No,
  NoOverflow,
  NoRetryLimitExceeded,
  Yes,
  NoRuntime,
};
```

declared in [`envoy/router/router.h`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/router.h#L345). The router
maps `NoOverflow` and `NoRetryLimitExceeded` onto the `UpstreamOverflow` and
`UpstreamRetryLimitExceeded` response flags, which is how a retry that was *wanted* but
refused becomes visible in the access log. The concurrency limit is the retry circuit
breaker, optionally a percentage of active requests via
[`RetryBudgetImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/resource_manager_impl.h#L122)
(see [Chapter 8](./08-upstream.md)).

When permission is granted, `shouldRetry` either schedules the callback on the next
dispatcher iteration or arms a jittered exponential backoff timer in
[`enableBackoffTimer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/retry_state_impl.cc#L158)
— or, if the response carried a rate-limit reset header, a one-shot strategy derived from
it. The callback lands in
[`Filter::doRetry`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2363), which bumps the
attempt count, re-resolves the cluster (CDS can have removed it mid-stream), selects a new
host and builds a fresh `UpstreamRequest`.

Host selection for a retry is itself pluggable.
[`RetryHostPredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/retry.h#L75) implementations veto
candidate hosts — `RetryStateImpl::shouldSelectAnotherHost` polls all of them, and the load
balancer re-picks a host up to `hostSelectionMaxAttempts()` times. The shipped predicates are
[`PreviousHostsRetryPredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/retry/host/previous_hosts/previous_hosts.h#L7),
[`OmitCanaryHostsRetryPredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/retry/host/omit_canary_hosts/omit_canary_hosts.h#L7)
and one that matches host metadata; a `RetryPriority` extension such as
[`PreviousPrioritiesRetryPriority`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/retry/priority/previous_priorities/previous_priorities.h#L13)
instead reshapes the priority load so the retry lands in a different tier.

Hedging is the variant where the original attempt is not abandoned. With
`hedge_on_per_try_timeout`, a per-try timeout runs
[`Filter::onSoftPerTryTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1400)
instead of resetting the stream: a second attempt is launched while the first stays in
`upstream_requests_`, and whichever returns headers first wins via `resetOtherUpstreams`.

## Internal redirect: replaying the downstream half

An internal redirect has the same "try again" shape as a retry, but resets a different
part of the machine. A retry keeps the downstream stream and builds a new
`UpstreamRequest`; an internal redirect throws away the response and replays the request
through a brand-new *downstream* filter chain. Stream recreation is the only mechanism in
Envoy by which one client request runs the decoder chain twice, which is worth holding in
mind against the single-pass model of [Chapter 6](./06-http-connection-manager.md).

`onUpstreamHeaders` reaches it when the route's redirect policy is enabled and the status
code matches:
[`Filter::setupRedirect`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2168)
refuses unless the whole request was received, its buffer never overflowed and a `Location`
header is present, then hands that header to
[`convertRequestHeadersForInternalRedirect`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L2197).
That function checks the budget first — a `num_internal_redirects` counter kept in the
stream's filter state at `LifeSpan::Request` and compared against `maxInternalRedirects()`
— then rewrites scheme, host and path on `downstream_headers_`, clears the route cache and
re-resolves the route, and asks every
[`InternalRedirectPredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/internal_redirect.h#L17)
whether the new route is acceptable — the shipped ones are
[`PreviousRoutesPredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/internal_redirect/previous_routes/previous_routes.h#L12),
[`SafeCrossSchemePredicate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/internal_redirect/safe_cross_scheme/safe_cross_scheme.h#L12)
and an allow-list. Only once those gates pass does it downgrade a non-`GET`/`HEAD` 303 to
`GET`, drop `Content-Length` and drain any decoding buffer (the code cites RFC 7231 6.4.4),
and save the original scheme, host and path in `x-envoy-original-url`; a `Cleanup` restores
the untouched headers on every earlier exit. Each rejection reason has its own
`passthrough_internal_redirect_*` counter, and any of them means the 3xx is simply proxied
downstream.

The stream swap itself is
[`ActiveStreamDecoderFilter::recreateStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1890),
which bails out unless the stream has `observedEndStream()`, aborts both filter chains,
stamps the `internal_redirect` response-code detail on the dying `StreamInfo`, copies the
discarded response headers into it so the access log still records them, and calls
[`ActiveStream::recreateStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L2525):

```cpp
  state_.is_internally_destroyed_ = true;
  connection_manager_.doEndStream(*this, /*check_for_deferred_close*/ false);

  RequestDecoder& new_stream = connection_manager_.newStream(*response_encoder, true);
```

The same `ResponseEncoder` is reused, so the client sees one stream throughout. The two
flags in [`conn_manager_impl.h`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.h) are the seam:
`is_internally_destroyed_` keeps the old stream out of the premature-reset accounting and
makes `canDestroyStream()` true so it does not linger as a zombie, while
`is_internally_created_` on the replacement
tells `decodeHeaders` to skip XFF/original-IP sanitization and tracing-header mutation,
which must happen only on the first pass. Downstream-only fields — start time, protocol,
bytes received, downstream timing — cross over via
[`setFromForRecreateStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/stream_info_impl.h#L427),
and the filter state is inherited: if the old stream's state holds anything at or above
`LifeSpan::Request`, the new stream's `FilterChain` span is rebuilt on top of the old
stream's parent span, so the `Request`-scoped redirect counter survives. Two other in-tree
extensions use the same door: the on-demand filter, which recreates the stream once an
on-demand VHDS update supplies the missing route, and the custom-response
[`RedirectPolicy`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/http/custom_response/redirect_policy/redirect_policy.h#L43);
the dynamic-modules ABI exposes it to out-of-tree filters as well.

## The timeout family

A proxied stream runs under several independent clocks, armed by different owners.

```
downstream                                             upstream
  |--- request headers -------------------------------------->|
  |  [stream idle timeout]      ActiveStream                   |
  |  [request timeout]          ActiveStream                   |
  |  [max stream duration]      ActiveStream                   |
  |                             [route timeout]   Router::Filter
  |                             [per-try timeout] UpstreamRequest
```

The downstream three are created in the `ActiveStream` constructor. The stream idle timer
is a *scaled* timer, so the overload manager can shrink it under pressure; rearmed on
essentially every byte in either direction, it fires
[`ActiveStream::onIdleTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1059),
sets the `StreamIdleTimeout` flag and sends a local reply. The request timeout bounds only
receipt of the complete request and is disarmed as soon as decoding finishes or the response
starts. Max stream duration is recomputed once the route is known by
[`refreshDurationTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1716),
which lets a route override the HCM value and lets a `grpc-timeout` request header, capped
and offset by config, become the stream's deadline.

On the upstream side,
[`FilterUtility::finalTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L180)
resolves the route, per-try and per-try-idle timeouts from the route entry, the `grpc-timeout`
header and the `x-envoy-upstream-rq-timeout-ms` family. The route timeout is armed when the
downstream request completes, not when it starts, and fires
[`Filter::onResponseTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1352),
which resets every in-flight attempt. Per-try timers belong to the individual attempt,
created in
[`UpstreamRequest::setupPerTryTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L558);
[`UpstreamRequest::onPerTryTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L584)
drops the per-try idle timer and then deliberately does nothing once the downstream response
has started, letting a slow body run against the global timeout instead. The upstream's own max stream duration, armed on pool
readiness, routes through
[`Filter::onStreamMaxDurationReached`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1477).

## Resets

A reset is the codec's way of saying that a stream ended without an orderly response. The
taxonomy in [`StreamResetReason`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/stream_reset_handler.h#L13)
distinguishes local from remote, refused-stream from ordinary reset, and connection failure
from connection termination, and adds `Overflow` for circuit breaking, `ProtocolError`,
`ConnectError`, `OverloadManager`, `Http1PrematureUpstreamHalfClose`, and
`RemoteResetNoError` — the RFC 9113 case where a peer resets *after* a complete response and
the response must not be discarded.

Upstream resets arrive at
[`UpstreamRequest::onResetStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L502),
which translates the reason into a response flag via
[`Filter::streamResetReasonToResponseFlag`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1745),
then calls [`Filter::onUpstreamReset`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1631).
There the router tries a retry through
[`maybeRetryReset`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1574) — which
refuses if the response has already started or if this attempt was already retried — and
otherwise synthesizes a 503 (or 502 for a protocol error) through
[`onUpstreamAbort`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1552), whose body
names the reset reason. Downstream resets take the mirror path through
[`ActiveStream::onResetStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L2098),
which records `DownstreamProtocolError` or `DownstreamRemoteReset` and destroys the stream.

## The record: StreamInfo and access logs

Everything above writes to one object: the
[`StreamInfo`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L736)
built with the `ActiveStream` when the request arrived (see
[Chapter 6](./06-http-connection-manager.md)). What matters here is its shape at the end,
because that is what an access logger reads: `onRequestComplete()` latches the final time
exactly once, and the flags accumulated from the
[`CoreResponseFlag`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/stream_info/stream_info.h#L40)
enum become the log's failure vocabulary —
[`ResponseFlagUtils`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/stream_info/utility.h#L50)
renders them as the short codes `UT` (upstream request timeout), `UF` (upstream connection
failure) and `URX` (retry limit exceeded) that appear in `%RESPONSE_FLAGS%`.

An access logger is an [`AccessLog::Instance`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/access_log/access_log.h#L81)
with a single `log(context, stream_info)` method, built by
[`AccessLogFactory::fromProto`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/access_log/access_log_impl.cc#L334).
Most implementations derive from
[`ImplBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/access_loggers/common/access_log_base.cc#L11),
which evaluates an optional [`AccessLog::Filter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/access_log/access_log.h#L66)
(status code, duration, response flag, gRPC status, log type, and boolean combinators) before
emitting. The line itself comes from a
[`Formatter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/formatter/substitution_formatter.h#L26): a parsed list of
providers, each pulling one field out of the context or the stream info, with commands such
as `%RESPONSE_FLAGS%` and `%UPSTREAM_HOST%` resolved through the table built by
[`getKnownStreamInfoFormatterProviders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/formatter/stream_info_formatter.cc#L1056).

Logs are emitted at typed points, not just at the end.
[`ActiveStream::log`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1020)
runs with `DownstreamStart` if the HCM config asks to flush on new requests, periodically with
`DownstreamPeriodic` if a flush interval is configured, and with `DownstreamEnd` during
stream destruction; each `UpstreamRequest` logs
independently through
[`UpstreamRequest::upstreamLog`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L270),
so a retried request produces several `UpstreamEnd` entries against one `DownstreamEnd`.

## Teardown

When the encoder chain finishes, `endStream()` runs
[`ConnectionManagerImpl::doEndStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L269).
If the response encoder is still attached and the downstream request was never fully
received, Envoy resets the codec stream rather than waiting; otherwise it goes straight to
[`doDeferredStreamDestroy`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L331).
That function disables the stream's timers, then consults

```cpp
    bool canDestroyStream() const {
      return state_.on_reset_stream_called_ || state_.codec_encode_complete_ ||
             state_.is_internally_destroyed_;
    }
```

from [`conn_manager_impl.h`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.h#L402).
If the codec has not yet confirmed it is done with the stream, the `ActiveStream` becomes a
*zombie*: it stays alive, expecting nothing but a codec notification, and is reaped later by
[`onCodecEncodeComplete`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L2165)
or [`onCodecLowLevelReset`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L2189).
Otherwise the stream runs `completeRequest()`, notifies filters via `onStreamComplete()`,
writes the `DownstreamEnd` access log (HTTP/3 defers this until the QUIC layer sees the data
acknowledged), destroys the filters, and hands itself to `dispatcher_->deferredDelete`.

The deferral is not decoration. `doDeferredStreamDestroy` is routinely reached from inside a
codec callback running on the `ActiveStream`'s own stack — a filter calling `encodeData`, a
reset delivered mid-parse — so freeing the object immediately would unwind into freed memory.
The same reasoning applies inside the router, where
[`UpstreamRequest::cleanUp`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L193)
defers the filter-manager callbacks because headers still owned by the upstream filter chain
would otherwise be destroyed while being iterated. The mechanism behind `deferredDelete` is
[Chapter 11](./11-event-loop-and-threading.md)'s subject.

Finally, the connection. A drain-manager vote (see [Chapter 1](./01-process-model.md)),
overload-driven keepalive disabling, or a
response that should not be followed by another all make `ActiveStream::encodeHeaders` start
draining. The first two go through
[`startDrainSequence`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L1692),
which sends a codec shutdown notice (a high-stream-ID GOAWAY on HTTP/2, a no-op on HTTP/1) and
arms a drain timer so that
[`onDrainTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L819)
can send the real GOAWAY later; the third does the same on HTTP/2 and HTTP/3 but on HTTP/1
moves the state machine straight to `Closing`, and any draining HTTP/1 response then gets a
`Connection: close` header. Either way the socket is closed by
[`checkForDeferredClose`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L255),
which acts only once the drain state has reached `Closing`, the stream list is empty and the
codec has nothing left to write — so
in-flight streams always get to finish.
