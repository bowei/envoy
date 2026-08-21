# 8. Upstream: Clusters, Load Balancing and Connection Pools

*Once a route has named a cluster, how does Envoy get from that name to a stream on
a real connection to a real host?*

Everything up to this point has been about a request arriving. This chapter is about it leaving.
Forwarding is not one decision but four — which *cluster*, which *host*, which *connection*, and
whether the cluster's budget allows any of it — and each gets its own replaceable abstraction:

```
Router::Filter          terminal decoder filter; owns the routing decision
    |  getThreadLocalCluster(name)
ThreadLocalCluster       worker-local view of one cluster
    |  chooseHost(context)
LoadBalancer             picks a Host out of the PrioritySet
    |  httpConnPool(host, ...)
ConnectionPool::Instance per (host, protocol, socket-option) pool of connections
    |  newStream(...)
ActiveClient -> CodecClient -> the wire
```

## The router filter is where the request stops going down

The router is an ordinary HTTP decoder filter, registered like any other extension (see
[Chapter 3](./03-extension-framework.md)) and iterated like any other by the filter manager (see
[Chapter 6](./06-http-connection-manager.md)),
with one distinguishing property: it never returns `Continue` from
[`Filter::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L477). It is the last
filter in every chain, and its job is to turn the decoded request into an upstream attempt.

`decodeHeaders` reads as a sequence of ways the request can fail before it ever leaves the process:
no matched route (local 404, the route itself having been resolved as described in
[Chapter 7](./07-routing.md)), no such cluster in
[`ClusterManager::getThreadLocalCluster`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/cluster_manager.h#L412),
`maintenanceMode()`, the control-plane-driven drop-overload percentage. Only then does it compute
timeouts (see [Chapter 9](./09-response-and-teardown.md)), finalize the request headers and call
`chooseHost` on the thread-local cluster.

Host selection is usually synchronous, but may instead return a cancellable handle, in which case
the filter returns `StopAllIterationAndWatermark` until
[`Filter::onAsyncHostSelection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L773)
fires. Either way the result reaches
[`Filter::createConnPool`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L971), which
consults the cluster's `upstreamConfig()` to find a
[`GenericConnPoolFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/router.h#L1687) — an indirection that exists
because a "connection pool" may be an HTTP pool, a TCP pool for `CONNECT` tunnelling, or a UDP
socket for `CONNECT-UDP`. The default,
[`HttpConnPool`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/upstreams/http/http/upstream_request.h#L21), just
wraps the handle returned by `ThreadLocalCluster::httpConnPool`.

[`Filter::continueDecodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L787)
then constructs an [`UpstreamRequest`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.h#L67)
— one *attempt*, so a retry or a hedge each gets its own — and calls
[`acceptHeadersFromRouter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L412),
which hits `newStream` on the pool immediately, before the upstream filter chain has run, so that
connection establishment overlaps request processing. A traffic shadow is the exception: rather
than an `UpstreamRequest`, the router hands a copy of the request to
[`ShadowWriter::shadow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L947)
or
[`streamingShadow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L950),
which sends it through `Http::AsyncClient` entirely outside this machinery (see
[Chapter 14](./14-async-client.md)) — which is exactly why a
mirrored response is discarded and can never influence retries or the downstream stream. That chain's terminal element,
[`UpstreamCodecFilter::decodeHeaders`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_codec_filter.cc#L54),
latches the headers and stalls if the pool has not called back yet. The pool answers with either
[`onPoolFailure`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L609),
translated into a synthetic stream reset, or
[`onPoolReady`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L634), which
stores the [`GenericUpstream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/router/router.h#L1627) handle and reports a
successful connect to outlier detection.

## A second filter chain, upstream

The `UpstreamRequest` does not hand the request to a codec directly. Its constructor builds an
[`UpstreamFilterManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L46) — a
subclass of the same
[`FilterManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.h#L702)
base the connection manager subclasses downstream as `DownstreamFilterManager`
(see [Chapter 6](./06-http-connection-manager.md)) — and runs the request
through it. This chain is per *attempt*, not per request: a retry builds a new one. Its filters
come from the first of three sources that yields any, each tried in turn: the cluster's
`http_filters`, declared in its
[HTTP protocol options](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/api/envoy/extensions/upstreams/http/v3/http_protocol_options.proto) and
served by
[`ClusterInfoImpl::createFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/upstream_impl.h#L1043);
the router's own `upstream_http_filters`; or
[`defaultUpstreamHttpFilterChainFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_codec_filter.h#L133),
which installs exactly one filter.

That one filter is
[`UpstreamCodecFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_codec_filter.h#L29), and
it is always present and always last — terminal for the upstream chain the way the router is
terminal for the downstream one. Requests reach it and are encoded to the codec; responses arrive
on its nested
[`CodecBridge`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_codec_filter.h#L56) and are pushed back
through the same chain in the encode direction.

Three differences from a downstream chain matter. `UpstreamFilterManager::streamInfo` delegates to
the *downstream* stream's, so upstream filters on every attempt read and write the one downstream
`StreamInfo` rather than the per-attempt one the `UpstreamRequest` itself
keeps. `sendLocalReply` is overridden to
emit through the downstream filter manager, so a local reply raised upstream is never seen by
upstream filters. And upgrade filter chains are unsupported: all three factories return false from
`createUpgradeFilterChain`.

## One cluster, one main-thread copy, N worker copies

The [`ClusterManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/cluster_manager.h#L251) owns clusters on the
main thread; workers never touch those objects. Instead each worker holds a
`ThreadLocalClusterManagerImpl` containing a
[`ClusterEntry`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/cluster_manager_impl.h#L600) per cluster,
and `ClusterEntry` is what implements
[`ThreadLocalCluster`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/thread_local_cluster.h#L79). Each entry has
its own `PrioritySetImpl`, its own load balancer instance, and its own connection pools. What is
shared is shared by `shared_ptr` and treated as immutable: `ClusterInfo`, host objects, and the
`HostVector`s themselves — the one mutable exception being each `Host`'s atomic health-flag
bitmask, discussed below.

Updates flow one way. When a cluster's membership changes,
[`ClusterManagerImpl::postThreadLocalClusterUpdate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/cluster_manager_impl.cc#L1177)
snapshots the new per-priority host vectors and posts them to every worker, which swaps them into
its own priority set — the read-copy-update idiom of
[Chapter 11](./11-event-loop-and-threading.md), at its largest scale in the codebase. Clusters a
worker has never used are deferred — the
update is stored as a "cluster initialization object" and inflated lazily by
`initializeClusterInlineIfExists`.

## Hosts, priorities and localities

A cluster's endpoints live in a [`PrioritySet`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/upstream.h#L553): a vector
of [`HostSet`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/upstream.h#L435)s indexed by priority level. Each `HostSet`
exposes the same host list sliced several ways — all hosts, healthy, degraded, excluded — and each
of those again bucketed by locality via
[`HostsPerLocality`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/upstream.h#L383). Those slices are precomputed
on update, not on every request, which is the whole point of the structure.

A [`Host`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/upstream.h#L143) carries an address, a weight, a transport socket
factory and an atomic bitmask of health flags — `FAILED_ACTIVE_HC`, `FAILED_OUTLIER_CHECK`,
`FAILED_EDS_HEALTH`, `DEGRADED_ACTIVE_HC` and others — which collapse into a coarse
`Healthy`/`Degraded`/`Unhealthy` verdict. Only the flags are mutable and shared across threads; the
slicing into healthy and degraded vectors happens on the main thread and is republished.

How the host list is produced is the cluster *type*, and each type is an extension under
`source/extensions/clusters/`.
[`StaticClusterImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/static/static_cluster.h#L17)
takes endpoints straight from config.
[`StrictDnsClusterImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/strict_dns/strict_dns_cluster.h#L18)
re-resolves names on a timer and makes one host per resolved address.
[`LogicalDnsCluster`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/logical_dns/logical_dns_cluster.h#L39)
keeps a *single* logical host whose current address is swapped underneath it, so a connection pool
can hold long-lived connections to many real IPs.
[`EdsClusterImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/eds/eds.h#L31) subscribes to the
control plane (see [Chapter 2](./02-configuration-and-xds.md)). [`OriginalDstCluster`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/original_dst/original_dst_cluster.h#L70)
synthesises hosts from the downstream connection's original destination. Types that need another
cluster up first declare `InitializePhase::Secondary`; the rest are `Primary`, and the two groups
initialize in order.

## Where a hostname becomes an address

The two DNS cluster types resolve on the main thread on a timer, and differ in what they do with
the answer. `StrictDnsClusterImpl` keeps one
[`ResolveTarget`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/strict_dns/strict_dns_cluster.h#L34) per
configured name; its
[`startResolve`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/strict_dns/strict_dns_cluster.cc#L130) makes
a host out of every distinct returned address, diffs that set against the current one with
`updateDynamicHostList`, and rearms the timer at `dns_refresh_rate` — or, with `respect_dns_ttl`,
at the smallest TTL in the response floored by `dns_min_refresh_rate` — plus a random `dns_jitter`
either way, or at an interval from the failure backoff strategy if the query failed.

LOGICAL_DNS keeps a single host forever. Its
[`startResolve`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/logical_dns/logical_dns_cluster.cc#L133)
takes the first address of the response as the host's address — keeping the rest as an address
list — and calls
[`setNewAddresses`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/common/logical_host.h#L41) on a
[`LogicalHost`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/clusters/common/logical_host.h#L18), whose address
members are lock-protected and re-read on each `createConnection`. Membership never churns and
existing connections to older addresses survive, which is what makes the type suitable for large
round-robin DNS names.

Resolution itself is [`Network::DnsResolver`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/dns.h#L105): one `resolve`
call, a cancellable query handle, and a callback carrying `Completed` or `Failure` along with
addresses and TTLs. The implementation is an extension selected by a
[`DnsResolverFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/dns_resolver.h#L16) —
[c-ares](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/network/dns_resolver/cares/dns_impl.h#L47) by default,
[Apple's](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/network/dns_resolver/apple/apple_dns_impl.h#L74)
`DNSServiceGetAddrInfo` by default on macOS, or a
[`GetAddrInfoDnsResolver`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/network/dns_resolver/getaddrinfo/getaddrinfo.h#L31)
that runs blocking `getaddrinfo` on dedicated resolver threads when the system resolver's exact
semantics are wanted. Dynamic forward proxy does not use a DNS cluster type at all: it resolves
hostnames seen in request headers through a shared
[`DnsCache`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/common/dynamic_forward_proxy/dns_cache.h#L126) and
synthesises hosts from its entries.

## Choosing a host

[`LoadBalancer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/load_balancer.h#L202) is essentially one method,
`chooseHost`, driven by a
[`LoadBalancerContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/load_balancer.h#L79) that the router
filter itself implements. The context is how request-scoped information reaches an otherwise
request-agnostic algorithm: the hash key for consistent hashing, metadata match criteria for subset
selection, the downstream connection, and `shouldSelectAnotherHost`, which keeps a retry off the
host that just failed.

Most policies derive from
[`ZoneAwareLoadBalancerBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/load_balancing_policies/common/load_balancer_impl.h#L262),
whose [`chooseHost`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/load_balancing_policies/common/load_balancer_impl.cc#L658)
is a retry loop around a subclass's `chooseHostOnce`. Before the subclass ever sees a host list, the
base class has done two things. First, priority selection: `recalculatePerPriorityState` in
[`LoadBalancerBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/load_balancing_policies/common/load_balancer_impl.h#L135)
computes, per priority, an availability figure scaled by the overprovisioning factor, and load
spills from priority 0 down as availability drops. If too few hosts are healthy the level enters
*panic mode* and traffic is spread over all hosts regardless of health, on the theory that a
partially broken backend beats no backend. Second, locality selection: if a local cluster is
configured, `regenerateLocalityRoutingStructures` compares — on membership change, not per request
— the fraction of local-cluster hosts in this zone against the fraction of upstream hosts in the
same zone, and latches either `LocalityDirect` or `LocalityResidual`; `tryChooseLocalLocalityHosts`
then either routes entirely locally or samples one of the residual localities. Both steps yield a
`HostsSource` — a (priority, slice, locality) cursor.

Weighted policies then go through
[`EdfLoadBalancerBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/load_balancing_policies/common/load_balancer_impl.h#L514),
which keeps one [`EdfScheduler`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/edf_scheduler.h#L28) per
`HostsSource`. Earliest-deadline-first gives weighted round robin with arbitrary floating-point
weights at O(log n) per pick:

```cpp
  void add(double weight, std::shared_ptr<C> entry) override {
    ASSERT(weight > 0);
    const double deadline = current_time_ + 1.0 / weight;
    EDF_TRACE("Insertion {} in queue with deadline {} and weight {}.",
              static_cast<const void*>(entry.get()), deadline, weight);
    queue_.push({deadline, order_offset_++, entry});
    ASSERT(queue_.top().deadline_ >= current_time_);
  }
```

Round robin and least request are both EDF subclasses differing in `hostWeight` — least request
folds in the host's active request count — and slow start is a weight multiplier applied by the same
base class. When all original weights are equal and no host is in slow start the scheduler is
skipped in favour of each subclass's cheaper `unweightedHostPick`: a rotating index for round robin,
a scan or N-choices sample for least request.

Consistent-hashing policies differ: ring hash and Maglev build a large lookup table that is
expensive to construct and identical on every worker, so they are
[`ThreadAwareLoadBalancer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/load_balancer.h#L328)s, built once
on the main thread and shared. The subset load balancer
([`SubsetLoadBalancer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/load_balancing_policies/subset/subset_lb.h#L35))
is a wrapper rather than an algorithm: it keeps a child load balancer per metadata subset and
delegates to whichever the request's match criteria select.

## Keeping the host list honest

Two independent mechanisms remove hosts from rotation. Active health checking, rooted in
[`HealthCheckerImplBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/health_checkers/common/health_checker_base_impl.h#L44),
runs a per-host session on the main thread dispatcher with interval and timeout timers; consecutive
results are counted against `unhealthy_threshold_` and `healthy_threshold_` before
[`handleSuccess`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/health_checkers/common/health_checker_base_impl.cc#L295)
or `handleFailure` toggles `FAILED_ACTIVE_HC`. The concrete checkers (HTTP, TCP, gRPC) are
extensions selected by
[`HealthCheckerFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/health_checker_impl.h#L94).

Outlier detection is passive: it infers health from live traffic. The router reports every outcome
through
[`Filter::updateOutlierDetection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/router.cc#L1502),
which calls `putResult` on the host's
[`DetectorHostMonitor`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/outlier_detection.h#L53), home to the
consecutive-5xx, local-origin-failure and success-rate accumulators. Those counters are touched
from all workers, so a detected outlier is signalled to the main thread by `dispatcher_.post`, and
[`DetectorImpl::ejectHost`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/outlier_detection_impl.cc#L542)
sets `FAILED_OUTLIER_CHECK` with a jittered, backing-off ejection time, refusing to eject beyond a
configured percentage of the cluster.

Both converge on the same place:
[`ClusterImplBase::setHealthChecker`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/upstream_impl.cc#L1969)
and `setOutlierDetector` register callbacks that call `reloadHealthyHosts`, which recomputes the
healthy and degraded slices and republishes them to every worker through the same posting path as a
membership update.

## Connection pools

Pools are per worker, per host, per resource priority, and per *hash key*: the key mixes the
upstream protocol, socket options and transport socket options, computed in
[`ClusterEntry::httpConnPoolImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/cluster_manager_impl.cc#L2028),
so that streams with incompatible connection requirements can never share a connection.

All pools share [`ConnPoolImplBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.h#L183),
which sorts its clients into per-state lists driven by a small state machine:

```cpp
  enum class State {
    Connecting,        // Connection is not yet established.
    ReadyForEarlyData, // Any additional early data stream can be immediately dispatched to this
                       // connection.
    Ready,             // Additional streams may be immediately dispatched to this connection.
    Busy,              // Connection is at its concurrent stream limit.
    Draining,          // No more streams can be dispatched to this connection, and it will be
                       // closed when all streams complete.
    Closed             // Connection is closed and object is queued for destruction.
  };
```

[`newStreamImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.cc#L327)
attaches to a ready client if one exists, otherwise queues a pending stream and calls
[`tryCreateNewConnections`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.cc#L166).
Preconnect lives here too: even on a cache hit the pool may open an additional connection in
anticipation of the next stream.

HTTP/1 and HTTP/2 differ in exactly one interesting parameter. The HTTP/1
[`ActiveClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http1/conn_pool.h#L18) is
[constructed](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http1/conn_pool.cc#L82) with a
concurrent stream limit of 1, so a client goes `Ready` → `Busy` → `Ready` per request; the
[HTTP/2 client](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/conn_pool.cc#L41) starts at the
configured `max_concurrent_streams` and is lowered to the peer's value when
[`onSettings`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_base.cc#L130)
sees the SETTINGS frame. Both pools are
the same [`FixedHttpConnPoolImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_base.h#L176)
parameterised by two lambdas: one to build the client, one to build the codec
([`Http1::allocateConnPool`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http1/conn_pool.cc#L109),
[`Http2::allocateConnPool`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/conn_pool.cc#L54)).

HTTP/3 needs more than a parameter, because QUIC may be blocked on the path. The
[`ConnectivityGrid`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_grid.h#L31) wraps HTTP/3 pools
and an HTTP/2 pool behind one `Instance`. Its
[`newStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_grid.cc#L441) starts with the
HTTP/3 pool whenever HTTP/3 is allowed for this origin and stream — dropping straight to the HTTP/2
pool when it is not — and then either waits before racing a TCP attempt or, if the cached HTTP/3
status for this origin shows recent failure, starts the TCP attempt at once. A per-attempt wrapper
relays cancellation to every attempt and reports failure upward only once all pools have been
tried.

## Establishing the upstream connection

The socket appears inside the `ActiveClient` constructor. When
[`tryCreateNewConnection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.cc#L186)
instantiates an
[`ActiveClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_base.h#L116), that client
calls [`createConnection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/upstream.h#L204) on the host.
[`HostImplBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/upstream_impl.h#L380) picks a transport socket
factory — normally the host's own, or one re-resolved per connection by the cluster's
[`TransportSocketMatcherImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/transport_socket_match_impl.h#L37)
when the match depends on filter state — asks for a source address, and creates the
`ClientConnection`. A host with several addresses gets a
[`HappyEyeballsConnectionImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/happy_eyeballs_connection_impl.h#L76)
that races address families instead.

Source addresses come from `upstream_bind_config`, on the cluster or on the bootstrap;
[`createUpstreamLocalAddressSelector`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/upstream_impl.cc#L360)
decides which, the cluster's config replacing the bootstrap's outright rather than merging with it.
The default
[`DefaultUpstreamLocalAddressSelector`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/upstream/default_local_address_selector.h#L19)
then picks the configured local address whose IP version matches the endpoint's.

Upstream TLS is an
[`UpstreamTransportSocketFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/transport_socket.h#L306)
wrapping that connection, and what makes it *upstream* TLS is that the SNI and the SAN to verify
can be per-request. With `auto_sni` or `auto_san_validation` set in the cluster's upstream HTTP
protocol options, the router parses the request authority and stores it in filter state, from which
it becomes the `TransportSocketOptions` that
[`ClientContextImpl::newSsl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/client_context_impl.cc#L121) prefers over the
statically configured server name.

The connection is then wrapped in a
[`CodecClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/codec_client.h#L51), built by the pool's
[`createCodecClient`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_pool_base.h#L98) hook;
[`CodecClientProd`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/codec_client.h#L359) constructs the HTTP/1,
HTTP/2 or HTTP/3 client codec, unless the cluster configures a codec factory of its own.
`CodecClient` owns the connection, holds the list of active requests, and is what a pool client's
`newStream` eventually reaches; its
[`onEvent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/codec_client.cc#L105) resets every outstanding request when
the connection dies.

Connect failure and connect timeout are separate paths that must stay separate, because the reset
reason differs — even though the router folds all three back onto the one
`UpstreamConnectionFailure` response flag. The `ActiveClient` constructor arms `connect_timer_` for the
cluster's `connectTimeout()`;
[`onConnectTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.cc#L896) sets
`timed_out_` and closes, so by the time
[`onConnectionEvent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/conn_pool/conn_pool_base.cc#L563) sees a close
that arrived before the handshake completed, the cause is already recorded:

```cpp
      ConnectionPool::PoolFailureReason reason;
      if (client.timed_out_) {
        reason = ConnectionPool::PoolFailureReason::Timeout;
      } else if (event == Network::ConnectionEvent::RemoteClose) {
        reason = ConnectionPool::PoolFailureReason::RemoteConnectionFailure;
      } else {
        reason = ConnectionPool::PoolFailureReason::LocalConnectionFailure;
      }
```

Those three [`PoolFailureReason`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/common/conn_pool.h#L110)s reach every
pending stream, and the router turns each into a distinct reset reason (see
[Chapter 9](./09-response-and-teardown.md)).

## Circuit breakers

Circuit breakers are not a separate component; they are counters checked at a few choke points on
the pool paths above.
[`ResourceManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/upstream/resource_manager.h#L39) hands out a
[`ResourceLimit`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/common/resource.h#L15) for connections, pending requests,
active requests, retries and connection pools, per cluster and per priority. `newStreamImpl`
consults `pendingRequests()` before queueing, `attachStreamToClient` consults `requests()` before
dispatching, and `tryCreateNewConnection` asks `host_->canCreateConnection(priority_)`, which
consults the cluster's `connections()` budget — with a deliberate
exception: if the cluster's connection budget is exhausted but *this* host has no connections at
all, one is created anyway, so that pending streams queued against it are not stranded.

Overflow surfaces as `PoolFailureReason::Overflow`, which becomes a synthetic
`StreamResetReason::Overflow` and then a 503 with the `UpstreamOverflow` response flag. Such a reset
is deliberately *not* reported to outlier detection: the condition belongs to the cluster's budget,
not to the host, and ejecting a host for it would only shrink the cluster further.
