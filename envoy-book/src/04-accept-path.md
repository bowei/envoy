# 4. The Accept Path: Listeners, Sockets and Transport

*What happens between `accept()` returning a file descriptor and the first byte
reaching a network filter?*

A TCP segment with SYN set arrives on a bound port. Between that moment and the
first byte reaching a network filter, Envoy has to answer a series of questions:
which worker thread will own this connection, is it allowed in at all, what
protocol is being spoken, which of the configured filter chains applies, and
whether the bytes on the wire are the bytes the application will see. This
chapter follows that sequence.

The organising idea is *deferred commitment*. Envoy delays creating the
expensive, long-lived objects — the connection, its buffers, its filters — until
it has learned enough about the flow to pick the right ones. Between `accept()`
and the connection object sits a stage in which Envoy holds only a socket and is
free to inspect, redirect, or discard it.

## From configuration to bound sockets

A `Listener` in configuration becomes a
[`ListenerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.h#L208),
which is a main-thread object: it owns the parsed config, the listener filter
factories, the filter chain manager, and — crucially — the listen sockets
themselves. Workers never bind. How that object is warmed, swapped and drained
when LDS sends a new version is covered in
[Chapter 2](./02-configuration-and-xds.md); this chapter starts once it exists.

Binding happens through a
[`ListenSocketFactoryImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.h#L66),
one per listener address, created by
[`createListenSocketFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_manager_impl.cc#L1262)
with a count equal to `--concurrency`. How those N sockets relate to each other
is decided by [`BindType`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/listener_manager.h#L102):
`NoBind` for listeners that never bind (internal listeners and
`bind_to_port: false`), `NoReusePort` for a single socket that is `dup()`ed for
each worker, and `ReusePort` for N genuinely independent sockets bound to the
same address with `SO_REUSEPORT`. The
[constructor](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.cc#L95)
creates socket zero and then either duplicates it or creates the rest, each
fresh bind first asking the hot restart parent for an inherited file descriptor.

`SO_REUSEPORT` is the default for TCP on Linux, chosen by
[`getReusePortOrDefault`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.cc#L1231)
and force-disabled elsewhere. It matters beyond performance: the kernel gives
each such socket its own accept queue and assigns incoming connections to a queue
immediately. Closing one resets whatever is queued on it rather than migrating
it, which is why the factory's cloning constructor — used when a listener is
updated in place or left draining — always `duplicate()`s the existing sockets
instead of binding fresh ones. A brand-new listener still binds.

`listen()` is called last, by
[`doFinalPreWorkerInit`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.cc#L210),
immediately before workers start — so a socket is never accepting connections
that nobody is ready to serve.

## One listener, N workers

Each worker thread runs a
[`ConnectionHandlerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/connection_handler_impl.h#L31),
the per-worker registry of everything currently listening. When the listener
manager pushes a listener to the workers,
[`addListener`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/connection_handler_impl.cc#L40)
constructs, for each address, an
[`ActiveTcpListener`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_listener.h#L25)
holding that worker's socket — `getListenSocket(worker_index)` — and files it in
two maps, one keyed by listener tag and one by address string. The address-keyed
map exists so that a connection whose original destination has been recovered can
be re-dispatched to the listener that owns that address.

`ActiveTcpListener` bridges the generic accept machinery to Envoy's listener
semantics. The machinery itself is a
[`TcpListenerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/tcp_listener_impl.cc#L147),
constructed by
[`createListener`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/connection_handler_impl.cc#L395),
which registers a **level-triggered** read event on the listen socket. Level
triggering is deliberate: the accept loop bounds how many connections it takes
per event, so it must be re-woken while the queue is still non-empty.

## The accept loop

[`TcpListenerImpl::onSocketEvent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/tcp_listener_impl.cc#L63)
loops at most `max_connections_to_accept_per_socket_event` times, calling
`accept()` on the [`IoHandle`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/io_handle.h#L43) — Envoy's
abstraction over "a thing you can do socket syscalls on", which need not be a
kernel fd at all (see [Chapter 12](./12-buffers-and-io.md)). The bound keeps one busy listener from
starving the rest of the worker's event loop.

Each accepted handle passes two admission checks before anything else is built.
[`rejectCxOverGlobalLimit`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/tcp_listener_impl.cc#L21)
either allocates a slot from the overload manager's global downstream connection
resource or compares a process-wide counter against a runtime key; then a load
shed point and a random `reject_fraction_` allow graceful shedding under
pressure (see [Chapter 1](./01-process-model.md) for both). A rejection closes the handle and calls `onReject` on the
[`TcpListenerCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/listener.h#L336), which
only increments a counter — the peer sees a connection open and immediately
close.

Survivors are wrapped in an
[`AcceptedSocketImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/listen_socket_impl.h#L190)
carrying the local and remote addresses, and delivered to
[`ActiveTcpListener::onAccept`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_listener.cc#L80),
which applies the per-listener connection limit and hands off to
`onAcceptWorker`.

## Balancing across workers

With `SO_REUSEPORT` the kernel has already balanced: each worker accepts only
from its own queue. Without it — Unix domain sockets, Windows, or an explicit
configuration — every worker's event loop wakes on the same shared socket and
the distribution is whatever the kernel's thundering-herd resolution produces.

[`onAcceptWorker`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_listener.cc#L109)
therefore consults a
[`ConnectionBalancer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_balancer_impl.h#L35)
before doing any work. The default is
[`NopConnectionBalancerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_balancer_impl.h#L51),
which just bumps a counter and returns the current handler.
[`pickTargetHandler`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_balancer_impl.cc#L22)
on the exact balancer takes a mutex, scans every registered handler, and picks
the one with the fewest connections; if that is a different worker, the socket is
handed to [`post`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_listener.cc#L168),
which wraps it in a shared pointer (because `Dispatcher::post` copies its lambda)
and re-enters `onAcceptWorker` on the target thread with `rebalanced = true`.
Which balancer a listener gets is decided once, on the main thread, by
[`buildConnectionBalancer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.cc#L917);
the CPU-locality option installs no user-space balancer at all, relying instead
on a reuse-port BPF program attached as a socket option.

## UDP and QUIC: the other accept path

None of the above applies to QUIC: there is no `accept()`. A QUIC listener binds
a UDP socket, every packet for every connection arrives on it, and the
connection is something Envoy builds on top of that datagram stream. The socket
machinery is
[`UdpListenerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/udp_listener_impl.h#L20),
the UDP counterpart of `TcpListenerImpl`; its
[`handleReadCallback`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/udp_listener_impl.cc#L104)
drains the socket — GRO and `recvmmsg` where available — and calls `onData` once
per datagram.

Steering is where UDP diverges. A TCP connection belongs to whichever accept
queue the kernel chose at SYN time. A datagram has no such affinity: the
reuse-port hash is over the four-tuple, but a QUIC connection is identified by
its *connection ID* and survives address changes, so the tuple is the wrong key.
[`ActiveUdpListenerBase::onData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/active_udp_listener.cc#L47)
therefore asks a virtual `destination()` which worker owns the packet, and if it
is not this one hands it to the
[`UdpListenerWorkerRouter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/listener.h#L34):
[`deliver`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/udp_listener_impl.cc#L191)
looks that worker up under a reader lock and posts to its dispatcher.

[`ActiveQuicListener`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/active_quic_listener.h#L27)
overrides `destination()` to read the connection ID out of the packet — but
would rather not. At configuration time the factory asks the connection-ID
generator for a
[BPF socket option](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_quic_connection_id_generator_factory.h#L40)
that makes the kernel select the reuse-port socket by connection ID; when that
works, `kernel_worker_routing_` is set and
[`destination`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/active_quic_listener.cc#L255)
keeps every packet where it landed. User-space routing is the fallback, and the
comment there says as much: most packets then arrive on the wrong worker.

[`onDataWorker`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/active_quic_listener.cc#L172)
wraps the buffer in a `quic::QuicReceivedPacket` and hands it to
[`EnvoyQuicDispatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_quic_dispatcher.h#L57),
which owns the connection-ID-to-session map. A packet for a known ID goes to its
session; an Initial carrying a ClientHello makes QUICHE parse the CHLO and call
[`CreateQuicSession`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_quic_dispatcher.cc#L95),
the QUIC analogue of everything between `accept()` and `newConnection`: it
synthesises a connection socket, runs the QUIC listener filters, calls the same
`findFilterChain` described below, and installs the chain's network filters.

What it creates is an
[`EnvoyQuicServerSession`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_quic_server_session.h#L53),
which is both a `quic::QuicServerSessionBase` and, through
[`QuicFilterManagerConnectionImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/quic_filter_manager_connection_impl.h#L31),
a `Network::ConnectionImplBase`. That dual identity is the trick: to a filter, a
QUIC session *is* the connection, so filter chains and the HTTP connection
manager attach to it as they would to a `ServerConnectionImpl`. It is also why
an HTTP/3 codec is built in `onNewConnection` rather than from sniffed bytes
(see [Chapter 6](./06-http-connection-manager.md)) — the dispatcher has already
demultiplexed, and there is no transport socket in the path at all. The TLS 1.3
handshake runs in the crypto stream returned by
[`createEnvoyQuicCryptoServerStream`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/quic/crypto_stream/envoy_quic_crypto_server_stream.cc#L12)
— QUICHE's own `TlsServerHandshaker`, subclassed as
[`EnvoyTlsServerHandshaker`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_tls_server_handshaker.h#L16)
only when the session-ticket or keylog runtime guard is on — and its certificate
comes from
[`EnvoyQuicProofSource::GetCertChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/quic/envoy_quic_proof_source.cc#L21),
which runs `findFilterChain` a second time.
The rest of this chapter is about TCP.

## Listener filters and the peek loop

Now the deferred-commitment stage. The socket is wrapped in an
[`ActiveTcpSocket`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_socket.h#L28)
— a socket, a `StreamInfo`, and a list of listener filters, but no connection and
no buffers. `onSocketAccepted` on the
[`ActiveStreamListenerBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_stream_listener_base.h#L28)
populates the filter list via
[`createListenerFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.cc#L1090)
and starts iteration.

A [`ListenerFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/filter.h#L443) has three methods
that matter here:

```c++
  virtual FilterStatus onAccept(ListenerFilterCallbacks& cb) PURE;
  // ...
  virtual FilterStatus onData(Network::ListenerFilterBuffer& buffer) PURE;
  // ...
  virtual size_t maxReadBytes() const PURE;
```

[`continueFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_socket.cc#L124)
walks the list calling `onAccept`. A filter that can decide immediately returns
`Continue`. A filter that needs to see bytes returns `StopIteration` and declares,
via `maxReadBytes()`, how many it wants. That triggers
[`createListenerFilterBuffer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_socket.cc#L85),
which registers a read event and, on each wakeup,
[peeks](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/listener_filter_buffer_impl.cc#L55)
at the socket with `MSG_PEEK`.

`MSG_PEEK` is what makes the whole scheme work: the bytes stay in the kernel
receive queue, so the eventual transport socket and network filters see an
untouched stream. A filter that wants to consume prefix bytes must say so with
[`drain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/listener_filter_buffer_impl.cc#L31),
which re-reads them for real. The consequence for filter authors is that
[`ListenerFilterBuffer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/listener_filter_buffer.h#L14)
hands back the *same* prefix on every callback, growing as more arrives — the TLS
inspector's [`onData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/listener/tls_inspector/tls_inspector.cc#L150)
tracks how much it has already parsed and skips it each time. When it finishes
parsing a ClientHello it calls `setRequestedServerName`, `setRequestedApplicationProtocols`
and `setDetectedTransportProtocol("tls")` on the socket, then returns `Continue`.

If a filter asks for data that never comes, a timer started when the socket was
parked fires
[`onTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_socket.cc#L53),
which either proceeds without the filter's answer or drops the socket, depending
on `continue_on_listener_filters_timeout`.

Not every listener filter reads. The original-destination filter's
[`onAccept`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/listener/original_dst/original_dst.cc#L21)
queries `SO_ORIGINAL_DST` and calls `restoreLocalAddress`, which is a decision
about identity rather than content: when
[`newConnection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_tcp_socket.cc#L196)
sees a restored address and the listener is configured to hand off, it looks up a
listener bound to that address and re-enters *that* listener's `onAcceptWorker`,
abandoning the current one. Otherwise it defaults the detected transport protocol
to `raw_buffer` and commits.

## Choosing a filter chain

Committing means calling
[`ActiveStreamListenerBase::newConnection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/active_stream_listener_base.cc#L27),
whose first act is
[`findFilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/filter_chain_manager_impl.cc#L551).
The classic `FilterChainMatch` path is not a scan; it is a fixed cascade of
nested lookups in a strict order: destination port, destination IP (an LC-trie,
so CIDR ranges work), server name, transport protocol, application protocol,
direct source IP, source type, source IP, source port. Most levels fall back to
a catch-all entry if the specific one misses — the destination port is the
exception, since a matching port subtree that yields nothing goes straight to
the fallback chain rather than retrying port 0 — and server name additionally walks
[wildcard suffixes](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/filter_chain_manager_impl.cc#L616).
This ordering is why `server_names` only works if the TLS inspector has populated
the SNI first. Newer configurations can instead supply a generic matcher tree,
evaluated by
[`findFilterChainUsingMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/filter_chain_manager_impl.cc#L586).
That tree is the generic matching framework described in
[Chapter 3](./03-extension-framework.md), parameterised on the connection socket
rather than on a request.
No match and no default chain closes the connection with a
`no_filter_chain_match` counter and an access log entry.

## The connection and its transport socket

The chosen [`FilterChain`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/filter.h#L639) supplies two
things: a transport socket factory and a list of network filter factories. Only
now does Envoy build the real objects — a
[`TransportSocket`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/transport_socket.h#L122) from
`createDownstreamTransportSocket()`, then a `ServerConnectionImpl` via
[`createServerConnection`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/event/dispatcher_impl.cc#L153),
then the network filters (see [Chapter 5](./05-network-filters-and-codecs.md)).

```
  accept()  →  ActiveTcpSocket  →  findFilterChain  →  ConnectionImpl
              (socket only,        (uses what the      (buffers, filters,
               listener filters     filters learned)    transport socket)
               may peek/redirect)
```

The transport socket is the seam between "bytes on the wire" and "bytes the
filters see":

```c++
  virtual IoResult doRead(Buffer::Instance& buffer) PURE;
  // ...
  virtual IoResult doWrite(Buffer::Instance& buffer, bool end_stream) PURE;
```

[`ConnectionImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L69)
knows nothing about how those bytes are produced. It registers one read/write
file event, and
[`onReadReady`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L765)
calls `doRead` into its read buffer and acts on the returned
[`IoResult`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/transport_socket.h#L41): bytes processed, an
end-of-stream flag, an optional error code, and a
[`PostIoAction`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/post_io_action.h#L9) telling the
connection whether to stay open.

For plaintext the implementation is nearly transparent:
[`RawBufferSocket::doRead`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/raw_buffer_socket.cc#L16)
loops on `ioHandle().read()` until the kernel is drained or the read buffer hits
its configured limit (Chapter 12 covers that loop and the buffer it fills). For TLS,
[`SslSocket::doRead`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/ssl_socket.cc#L110)
first checks whether the handshake is complete and, if not, drives it — so the
handshake is not a separate phase but a state the read path passes through.
[`SslHandshakerImpl::doHandshake`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/ssl_handshaker.cc#L142)
calls `SSL_do_handshake` and maps BoringSSL's `WANT_READ`/`WANT_WRITE` into
"stay open, wait for the next event", and the four async errors — pending
certificate, offloaded private key operation, asynchronous certificate
validation, X509 lookup — into a blocked state. It is the wrapping
[`SslSocket::doHandshake`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/ssl_socket.cc#L208)
that read-disables the connection while that state holds, so that a peer close
is still noticed. On success,
[`onSuccess`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/ssl_socket.cc#L193) records
handshake timing and raises `ConnectionEvent::Connected`.

A server connection is TCP-connected from birth, so
[`initializeReadFilters`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L1074)
calls `onConnected()` on the transport socket only after the filters exist and
can observe the resulting event. A stuck handshake is bounded by
[`setTransportSocketConnectTimeout`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L1046),
armed from the filter chain's configured value.

Because the transport socket is just an interface, `starttls` and `proxy_protocol`
under `source/extensions/transport_sockets/` slot in exactly where TLS does; the
TLS factory itself is registered as
[`DownstreamSslSocketFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/transport_sockets/tls/downstream_config.h#L13)
around the core code in `source/common/tls/`.

## Which certificate does Envoy serve

A downstream TLS context is not one certificate.
[`ServerContextImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/server_context_impl.h#L47)
holds a vector of `Ssl::TlsContext`, one per configured certificate, and chooses
between them on every handshake. The hook is BoringSSL's
`select_certificate_cb`, installed on the first context and fired once the
ClientHello is parsed, before anything is signed. It lands in
[`selectTlsContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/server_context_impl.cc#L491),
which delegates to a pluggable `Ssl::TlsCertificateSelector`.

The default is
[`DefaultTlsCertificateSelector`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/default_tls_certificate_selector.h#L26).
Its [`findTlsContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/default_tls_certificate_selector.cc#L167)
tries the exact SNI against a map built from each certificate's DNS SANs (or its
subject CN if it has no SANs at all), then the wildcard suffix, then — only if
`full_scan_certs_on_sni_mismatch` allows — every certificate, and finally falls
back to the first one configured. Among candidates it prefers ECDSA when the
client advertised the certificate's curve, keeps an RSA certificate as a
fallback, and rejects any whose OCSP staple violates the configured policy. A
selector may also answer asynchronously: returning `Pending` makes
`selectTlsContext` return `ssl_select_cert_retry`, which is one of the blocked
states the previous section described.

ALPN is decided separately and later, by
[`alpnSelectCallback`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/server_context_impl.cc#L71),
in the server's configured priority order. Note the asymmetry: the TLS
inspector's SNI and ALPN chose the *filter chain*; these callbacks choose within
the context that chain named. Peer certificates go the other way, through a
[`CertValidator`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/cert_validator/cert_validator.h#L50)
on the context —
[`DefaultCertValidator`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/cert_validator/default_validator.h#L93)
for trust store, SAN matching and CRLs, SPIFFE and others as extensions — which
can likewise park the handshake. Stateless session tickets are sealed by
[`sessionTicketProcess`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/server_context_impl.cc#L338)
with the first configured key and opened with any of them, which is what makes
key rotation across a fleet work.

Contexts are owned by
[`ContextManagerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/context_manager_impl.h#L26),
the main-thread registry that builds every one through
[`createSslServerContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/context_manager_impl.cc#L38)
and keeps the set reachable for expiry stats and the admin `/certs` endpoint.
That closes the SDS loop from
[Chapter 2](./02-configuration-and-xds.md): a secret update calls
[`onAddOrUpdateSecret`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/tls/server_ssl_socket.cc#L79)
on the transport socket factory, which builds a whole new context and swaps it
under a write lock — established connections keep the old one, the next
`createDownstreamTransportSocket()` gets the new.
