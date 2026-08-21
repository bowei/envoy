# 12. Buffers, Flow Control and I/O

*Where do bytes live while they are in flight, and what stops a slow reader on one
side from exhausting memory on the other?*

A proxy is a machine for moving bytes between two sockets that run at different speeds. Two problems follow. Every byte copied is a byte not spent on something useful, so the structures holding bytes in flight must hand ownership around rather than memcpy. And if the reader on one side is slower than the writer on the other, an implementation that buffers whatever arrives accumulates memory until the process is killed. Envoy answers the first with a slice-based buffer and the second with watermarks that propagate a "stop" signal back to a `readDisable` on the originating socket.

## The buffer contract

Everything that holds bytes in Envoy is a [`Buffer::Instance`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L145), declared in [`envoy/buffer/buffer.h`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h). The interface is deliberately not a `std::string`: it exposes `length()`, `add()`, `drain()`, `move()`, `linearize()` and `getRawSlices()`, and it makes no promise that the bytes are contiguous. A caller that needs contiguity has to ask for it explicitly.

The unit of exchange with the outside world is [`RawSlice`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L30), a bare `{void* mem_, size_t len_}` pair shaped so that it can be reinterpreted as an `iovec`. `getRawSlices()` returns a vector of these pointing into the buffer's own storage; nothing is copied.

A buffer can also hold bytes it does not own. A [`BufferFragment`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L55) wraps external memory and has `done()` called on it once the last byte drains, which is how a codec emits a static string without a copy. Separately, `addDrainTracker()` registers a callback that fires when the bytes it was attached to are finally consumed, even if they have since moved into a different buffer.

Filling a buffer from a socket uses the reservation protocol. `reserveForRead()` returns a [`Reservation`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L589) holding up to `MAX_SLICES_` (eight) raw slices of writable memory; the caller reads into them and calls `commit(n)` with the number of bytes actually produced, and uncommitted space is discarded when the reservation dies. This is what lets a socket read land directly in buffer storage.

## Slices, and why move is cheap

The concrete implementation is [`OwnedImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.h#L643), which owns a running `length_` and a [`SliceDeque`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.h#L426) — a hand-rolled ring of eight inline entries that grows to a heap array, written because `std::deque` was measurably too slow. Each [`Slice`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.h#L37) is a block of storage with three offsets into it:

```
+---------+--------------+-------------+
| drained | data         | reservable  |
+---------+--------------+-------------+
^         ^              ^             ^
base_     base_+data_    base_+        base_+capacity_
                         reservable_
```

Draining from the front just advances `data_`, so `drain()` on a slice is O(1) and touches no memory. Appending advances `reservable_`. Capacity is rounded up to a multiple of 4 KB, the default slice is 16 KB, and a default read reservation is therefore 128 KB of writable space.

[`OwnedImpl::drainImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.cc#L184) pops whole slices off the front and only advances an offset for the final partial one. [`OwnedImpl::move`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.cc#L334) walks the source deque and transfers slice ownership to the destination, so moving a megabyte between two buffers costs a handful of pointer moves rather than a megabyte of copying. The nuance lives in [`coalesceOrAddSlice`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.cc#L312): if the source slice holds fewer than 512 bytes, owns its storage rather than referencing a fragment, and the destination's tail slice has room, the bytes are copied instead — transferring ownership of a mostly-empty 16 KB slice to save a 100-byte memcpy is a bad trade. The partial-length overload of `move()` also copies when it must split a slice, since slices are not reference counted.

[`linearize()`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/buffer_impl.cc#L289) is the escape hatch for code needing a contiguous pointer: if the first slice already holds `size` bytes it hands back a pointer into it, otherwise it allocates a slice, copies the first `size` bytes in, drains them from the front and pushes the new slice on. That fallback is a copy by construction, which is why parsers avoid it.

## Filling and draining buffers from sockets

The file descriptor is hidden behind [`IoHandle`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/io_handle.h#L43), declared in [`envoy/network/io_handle.h`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/io_handle.h). Its scatter/gather read is expressed directly in terms of the buffer's slice type:

```cpp
  virtual Api::IoCallUint64Result readv(uint64_t max_length, Buffer::RawSlice* slices,
                                        uint64_t num_slice) PURE;
```

[`IoSocketHandleImpl::read`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/io_socket_handle_impl.cc#L98) glues the two halves together: reserve, `readv`, commit exactly what came back. [`readv`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/io_socket_handle_impl.cc#L73) itself builds an `iovec` array from the slices and, when there is only one, calls `recv` instead to avoid the kernel's iovec handling. The write side mirrors it: [`IoSocketHandleImpl::write`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/io_socket_handle_impl.cc#L139) takes at most sixteen raw slices, hands them to `writev`, and drains what the kernel accepted. Socket options are applied through the same handle via `setOption`, with configuration-driven options expressed as [`Socket::Option`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/socket.h#L510) visitors that run at defined points in the socket's life.

Every syscall is reached through [`Api::OsSysCalls`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/api/os_sys_calls.h#L45), a pure virtual interface whose POSIX implementation in [`os_sys_calls_impl.cc`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/api/posix/os_sys_calls_impl.cc) does nothing but call the libc function and package `errno`; call sites reach it through [`OsSysCallsSingleton`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/api/posix/os_sys_calls_impl.h#L77). The indirection buys a single place to paper over Windows and Linux differences, feature probing (`supportsMmsg`, `supportsUdpGro`), and the ability for tests to inject failures at any syscall without a real socket.

Results come back as [`IoCallResult`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/api/io_error.h#L80), a return value paired with an [`IoError`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/api/io_error.h#L21) pointer that is null on success. Errno is mapped into a small platform-independent enum, so callers test `wouldBlock()` rather than comparing against `EAGAIN`. [`IoSocketError`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/io_socket_error_impl.h#L10) implements the mapping, and because `EAGAIN` dominates in a non-blocking event loop, [`getIoSocketEagainError`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/io_socket_error_impl.cc#L28) hands out a shared singleton with a no-op deleter instead of allocating.

Datagram sockets sit slightly to the side: sends go through a [`UdpPacketWriter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/udp_packet_writer_handler.h#L40), which the default [`UdpDefaultWriter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/udp_packet_writer_handler_impl.h#L12) satisfies with one unbatched write per packet — `sendmsg` with the peer address, or `writev` if the socket is already connected — while batching writers coalesce packets and report `isWriteBlocked()`.

## The write path on a connection

A caller of [`ConnectionImpl::write`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L590) does not perform a syscall. The data runs through the write filter chain (see [Chapter 5](./05-network-filters-and-codecs.md)), then `write_buffer_->move(data)` transfers the caller's slices into the connection's write buffer, and the connection asks the event loop to deliver a writable event — a `FileEvent::activate`, which schedules a callback for the next loop iteration (see [Chapter 11](./11-event-loop-and-threading.md)). The actual `writev` happens later in [`onWriteReady`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L866), which delegates to the transport socket's `doWrite` — for TCP, [`RawBufferSocket::doWrite`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/raw_buffer_socket.cc#L51), looping until the buffer empties or the kernel returns `EAGAIN`. The transport socket interface itself, and what TLS does behind it, is [Chapter 4](./04-accept-path.md)'s subject. Deferring means many small writes coalesce into one syscall, and partial writes need no special case: whatever the kernel did not take stays in the buffer for the next writable event.

Reads are the mirror image. [`RawBufferSocket::doRead`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/raw_buffer_socket.cc#L16) loops on `IoHandle::read` into the connection's read buffer, but yields when [`shouldDrainReadBuffer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.h#L150) reports the buffer has reached its configured limit, marking the transport readable again so the loop resumes after other connections have had a turn.

## Watermarks

Both connection buffers come from the dispatcher's [`WatermarkFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L543) and are therefore [`WatermarkBuffer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.h#L24) instances. A `WatermarkBuffer` subclasses `OwnedImpl` and overrides every method that can change the length, calling [`checkHighAndOverflowWatermarks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.cc#L144) after growth and [`checkLowWatermark`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.cc#L134) after shrinkage. The low watermark is implicitly half the high one — see [`setWatermarks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.cc#L119) — and the callbacks are edge-triggered with hysteresis:

```cpp
void WatermarkBuffer::checkLowWatermark() {
  if (!above_high_watermark_called_ ||
      (high_watermark_ != 0 && OwnedImpl::length() > low_watermark_)) {
    return;
  }

  above_high_watermark_called_ = false;
  below_low_watermark_();
}
```

Without the gap between the two thresholds, a buffer hovering at the limit would flap between paused and resumed on every packet. A third, optional overflow watermark fires once when the buffer exceeds a multiple of the high watermark, as a last-resort signal that back pressure is not working. The buffer also adjusts its own reads: [`WatermarkBuffer::reserveForRead`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.cc#L84) shrinks the reservation so a single `readv` cannot overshoot the high watermark by 128 KB.

Watermarks bound a single buffer. To bound a whole stream, whose bytes may be spread across a codec buffer, a filter buffer and a connection buffer, Envoy adds accounting: a [`BufferMemoryAccount`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/buffer/buffer.h#L107) is charged by each `Slice` as it allocates and credited when it is destroyed. [`BufferMemoryAccountImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.h#L95) sorts accounts into power-of-two size buckets, so when the overload manager (see [Chapter 1](./01-process-model.md)) reports memory pressure, [`resetAccountsGivenPressure`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/buffer/watermark_buffer.cc#L203) resets the largest streams first rather than picking arbitrarily.

## Backpressure as a whole-system property

Now follow a slow client downloading a large response. Every buffer between the upstream socket and the downstream socket is a place the bytes can pile up.

```
  bytes:   upstream socket -> router -> encoder filters -> codec stream
                                                        -> connection write buffer -> client

  signal:  write buffer / stream send buffer over high watermark
              -> ActiveStream::onAboveWriteBufferHighWatermark
              -> FilterManager::callHighWatermarkCallbacks
              -> DownstreamWatermarkCallbacks (registered by the router)
              -> UpstreamRequest::readDisableOrDefer
              -> upstream stream readDisable
              -> H1: socket read events off / H2: WINDOW_UPDATE withheld
```

The downstream write buffer fills first, because the kernel socket buffer is full and `onWriteReady` cannot drain it. Crossing the high watermark invokes [`onWriteBufferHighWatermark`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L703), which raises `onAboveWriteBufferHighWatermark` on the connection's [`ConnectionCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/network/connection.h#L44); for an HTTP connection the connection manager (see [Chapter 6](./06-http-connection-manager.md)) forwards that to the codec, which fans it out to every active stream. The same signal also arrives per-stream from the codec's own buffers: in HTTP/2 each stream has a `pending_send_data_` watermark buffer whose [`pendingSendBufferHighWatermark`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/codec_impl.cc#L741) calls [`runHighWatermarkCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/codec_helper.h#L30).

Either way the stream reaches [`ActiveStream::onAboveWriteBufferHighWatermark`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/conn_manager_impl.cc#L2141), which invokes [`FilterManager::callHighWatermarkCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_manager.cc#L1724) and thereby every registered [`DownstreamWatermarkCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/codec.h#L676). The router (see [Chapter 8](./08-upstream.md)) registers one of those; its handler reaches [`UpstreamRequest::readDisableOrDefer`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/router/upstream_request.cc#L773), which read-disables the upstream stream — unless the response has not started yet, in which case the disable is counted and applied once response headers arrive, so that a stalled downstream cannot get the upstream request spuriously reset or retried by an upstream timeout that fires while the upstream stream is read-disabled.

What "read-disable the upstream stream" means depends on the protocol, and this is where the loop closes on the wire. In HTTP/1 the stream is the connection: [`StreamEncoderImpl::readDisable`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http1/codec_impl.cc#L386) calls straight through to [`ConnectionImpl::readDisable`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L490), which stops asking the event loop for read events; the kernel receive buffer fills and TCP stops advertising window. In HTTP/2, [`StreamImpl::readDisable`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/codec_impl.cc#L538) increments a counter that [`ConnectionImpl::onData`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/codec_impl.cc#L1160) consults: while reads are disabled, received bytes accumulate in `unconsumed_bytes_` and no `WINDOW_UPDATE` is sent, closing the peer's flow control window. When the block clears, [`grantPeerAdditionalStreamWindow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/http2/codec_impl.cc#L529) marks those bytes consumed and the window reopens.

Independently, the connection's own read buffer has a high watermark, and [`onReadBufferHighWatermark`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L687) calls `readDisable(true)` directly. `readDisable` is reference counted precisely because these sources are independent: a stream and a connection can each block the same socket, and reading resumes only when every one of them has released. The limits come from configuration through [`setBufferLimits`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/connection_impl.cc#L641), which applies one value to both buffers. Nothing here is a hard cap; each hop is a soft limit that produces a signal, and the signals compose into a chain ending at a socket that stops being read.
