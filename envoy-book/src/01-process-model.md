# 1. What Envoy Is: Process and Threading Model

*What threads does a running Envoy have, what does each one own, and how do they
stay out of each other's way?*

A proxy sits between two parties that would otherwise talk directly and takes
responsibility for the conversation. At L4 that means accepting a TCP or UDP
flow and forwarding bytes somewhere; at L7 it means parsing the application
protocol — HTTP/1, HTTP/2, HTTP/3, gRPC — and making routing, retry, and
observability decisions per request rather than per connection. The hard part is
not the forwarding. It is that the set of listeners, routes, upstream clusters,
and endpoints changes continuously while traffic is flowing, and that none of
those changes may drop a connection.

Envoy's answer is a single self-contained process with no runtime dependencies,
which is configured almost entirely over the network (see
[Chapter 2](./02-configuration-and-xds.md)) and can be
replaced by a new binary without losing a connection. This chapter is about that
process: which threads exist, what each one owns, and how they avoid getting in
each other's way.

## The shape of a running Envoy

A serving Envoy has one *main thread* and N *worker threads*, plus a small number
of auxiliary threads that do not touch traffic.

The main thread owns everything that is global and mutable: the configuration,
the cluster manager, the listener manager, the admin endpoint, the stats flush
timer, signal handling. It carries no proxied traffic: it has a connection
handler of its own, but the only listener attached to it is the admin endpoint
(`admin_->addListenerToHandler(handler_.get())` in
[`InstanceBase::initializeOrThrow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L464)).

Each worker thread owns a slice of the data plane. Every accepted connection is
bound to exactly one worker for its lifetime, and all of the state that
connection touches — the filter chain, the codec, the connection pool it borrows
an upstream connection from — lives on that worker and is touched only by that
worker.

N defaults to `std::thread::hardware_concurrency()` and is set by `--concurrency`
in [`OptionsImpl::OptionsImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/options_impl.cc#L38).
The auxiliary threads are the guard dog threads described below, the access log
flush threads described next, plus whatever threads a bootstrap extension or
gRPC library spins up.

One more auxiliary thread is easy to miss, because nothing at startup creates it.
Every file the access log manager opens gets an
[`AccessLogFileImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/access_log/access_log_manager_impl.h#L69),
and the first write to that file lazily spawns a thread named `AccessLogFlush`
via `thread_factory_.createThread` in
[`createFlushStructures`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/access_log/access_log_manager_impl.cc#L228).
A worker logging a request does not touch the file: it appends to `flush_buffer_`
under `write_lock_` in
[`AccessLogFileImpl::write`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/access_log/access_log_manager_impl.cc#L213)
and returns, while
[`flushThreadFunc`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/access_log/access_log_manager_impl.cc#L133)
moves the buffer aside and performs the actual write. The thread exists because a
write to a regular file can block in the kernel even on a descriptor opened
`O_NONBLOCK`, and a blocked worker is a stalled event loop for every connection
it owns (see [Chapter 10](./10-one-request-end-to-end.md) for where this lands in
a request).

Two abstractions appear on every thread. [`Api::Api`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/api/api.h#L25) is
the process's handle on the operating system: thread factory, filesystem, clock,
random generator, and — importantly — `allocateDispatcher`. An
[`Event::Dispatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/event/dispatcher.h#L104) is one thread's
event loop: file events, timers, signals, and a `post()` queue for work
submitted from other threads. A thread in Envoy is, almost always, "a dispatcher
plus the objects that only it may touch."
[Chapter 11](./11-event-loop-and-threading.md) explains how the dispatcher
actually works; here it is enough to know that `run()` blocks until someone calls
`exit()`, and that `post()` is the only sanctioned way to reach another thread.

## From main() to the dispatch loop

[`source/exe/main.cc`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/main.cc) does almost nothing: it hands
`argc`/`argv` to
[`MainCommon::main`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/main_common.cc#L158), which
declares a [`Thread::MainThread`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/common/thread.h#L221)
RAII marker — this is what makes `ASSERT_IS_MAIN_OR_TEST_THREAD()` work
throughout the codebase — constructs a
[`MainCommon`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/main_common.h#L88) inside a try/catch, and
then calls `run()` *outside* the try/catch so that unexpected exceptions produce
a core dump rather than a swallowed error.

`MainCommon` parses flags into `OptionsImpl` and builds a
[`StrippedMainBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/stripped_main_base.h#L36), the
portion of startup that Envoy Mobile also uses. Its constructor runs before any
configuration is read: it disables extensions named on the command line, enables
core dumps, constructs the hot restarter via
[`configureHotRestarter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/stripped_main_base.cc#L106),
and creates the thread-local instance and the thread-local stats store. Note the
ordering — the hot restarter must exist first, because it owns the shared-memory
mutexes that the logger and access logger lock against the parent process.

Which work happens at all depends on
[`Server::Mode`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/options.h#L23): `Serve` runs the server,
`Validate` only parses the configuration through
[`validateConfig`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/config_validation/server.cc#L27),
and `InitOnly` exits after initialization.
[`StrippedMainBase::init`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/stripped_main_base.cc#L77)
invokes a factory — [`createFunction`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/main_common.cc#L34)
in the production build — that constructs an
[`InstanceImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/instance_impl.h#L10) and calls
`initialize()` on it.

[`InstanceBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.h#L240) is where the server
actually lives; `InstanceImpl` only supplies the handful of components that tests
and other embeddings want to override, such as the overload manager and the
guard dog. The `InstanceBase` constructor creates the `Api::Impl` and, from it,
the main thread's dispatcher, named `main_thread`.

[`InstanceBase::initializeOrThrow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L464)
is the long function that assembles the server, and its ordering constraints are
mostly dependency edges: register the main thread for thread-local updates first
so that any code may use TLS; load and validate the bootstrap; set up the stats
store's tag producer and matcher before anyone creates a stat; create the runtime
loader before the overload manager so resource monitors can read runtime keys;
create the listener manager — which is what actually constructs the worker
objects — before enabling threaded stats; then the cluster manager, LDS, the
stats sinks and flush timer, and finally the guard dogs. On POSIX it also raises
the file descriptor soft limit to the hard limit in
[`InstanceUtil::raiseFileLimits`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L399).

[`InstanceBase::run`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L1066) builds a
[`RunHelper`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L990) on the stack. The
`RunHelper` constructor installs signal handlers (`SIGTERM` and `SIGINT` call
`shutdown()`, `SIGUSR1` reopens access logs, `SIGHUP` is deliberately eaten),
starts the overload manager, and registers a callback on the cluster manager. The
sequencing that follows is the important part: workers do not start until the
cluster manager reports every cluster initialized and the top-level init manager
has finished — the init framework that tracks those dependencies is described in
Chapter 2 — at which point
[`InstanceBase::startWorkers`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L945)
runs. Because that initialization usually needs the event loop to make progress,
`run()` enters `dispatcher_->run(RunType::Block)` immediately after constructing
the `RunHelper` and the worker start happens from inside that loop; the main
thread then stays there until `shutdown()` calls `exit()`. Interested parties can observe the
transitions through the
[`ServerLifecycleNotifier`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/lifecycle_notifier.h#L11)
stages `Startup`, `PostInit`, and `ShutdownExit`.

## Main thread and workers: shared nothing

A worker is a dispatcher plus a connection handler, and nothing else:

```cpp
WorkerPtr ProdWorkerFactory::createWorker(uint32_t index, OverloadManager& overload_manager,
                                          OverloadManager& null_overload_manager,
                                          const std::string& worker_name) {
  Event::DispatcherPtr dispatcher(
      api_.allocateDispatcher(worker_name, overload_manager.scaledTimerFactory()));
  auto conn_handler = getHandler(*dispatcher, index, overload_manager, null_overload_manager);
  return std::make_unique<WorkerImpl>(tls_, hooks_, std::move(dispatcher), std::move(conn_handler),
                                      overload_manager, api_, stat_names_);
}
```

The [`WorkerImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/worker_impl.h#L49) constructor calls
`registerThread` on the thread-local instance *before* the OS thread exists —
registration is about the dispatcher, not the thread — and nearly every worker
method that touches the connection handler is a one-line `dispatcher_->post()`
(`numConnections()` is the exception, reading the handler's counter directly).
[`WorkerImpl::start`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/worker_impl.cc#L112) then
creates the actual thread, named `wrk:worker_N`, running
[`threadRoutine`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/worker_impl.cc#L168),
which posts a startup callback and blocks in `run()`.
[`ListenerManagerImpl::startWorkers`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_manager_impl.cc#L1065)
starts all of them and blocks on an `absl::BlockingCounter` until each is
actually running.

Envoy is *shared-nothing* rather than lock-based, and the mechanism is
[`ThreadLocal::Slot`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/thread_local/thread_local.h#L21). A component on
the main thread allocates a slot and fills it with an object built per thread:

```cpp
  using InitializeCb = std::function<ThreadLocalObjectSharedPtr(Event::Dispatcher& dispatcher)>;
  virtual void set(InitializeCb cb) PURE;
```

Reads on a worker are a plain vector index into a `thread_local` array — no
atomics, no locks — while writes are posted to every registered dispatcher and
applied by each worker at a safe point in its own event loop, with the old value
kept alive by `shared_ptr` for anyone mid-request. That is read-copy-update, and
it is what lets a route table be swapped while requests are in flight; the
mechanics are Chapter 11's subject. The main thread is registered as a
thread-local thread too (with `main_thread = true` in
[`InstanceImpl::registerThread`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/thread_local/thread_local_impl.cc#L138)),
so code that does not care where it runs can use TLS unconditionally.

The consequence, and the reason this chapter comes first: for the rest of the
book, "who owns this object and on which thread" is usually the whole answer.
Configuration objects are immutable once published and are replaced wholesale.
Cross-thread communication is `post()` and thread-local slots, not mutexes. When
you find a lock in core Envoy — the guard dog's, the shared-memory log lock — it
is worth asking why, because the default is that there isn't one.

Shutdown reverses this carefully.
[`InstanceBase::terminate`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L1097)
calls `shutdownGlobalThreading` and `shutdownThreading` on the thread-local
instance and the stats store before
[`ListenerManagerImpl::stopWorkers`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_manager_impl.cc#L1191)
joins the worker threads, so that no cross-thread post can be issued to a thread
that is going away. Each worker destroys its connection handler on its own thread
in `threadRoutine` for the same reason: destructors must not run on the main
thread if they touch thread-local state.

## Keeping the threads honest: watchdogs and the guard dog

Because a worker's event loop is cooperative, one blocking call inside a filter
stalls every connection on that thread. Envoy detects this with a
[`WatchDog`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/watchdog.h#L17) per event loop, touched by a
timer the dispatcher owns, and a
[`GuardDog`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/guarddog.h#L19) that scans them.

[`GuardDogImpl::start`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/guarddog_impl.cc#L222)
launches a dedicated thread (named `dog:...`) whose only job is to run
[`step`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/guarddog_impl.cc#L106) periodically.
`step` walks the registered watchdogs under a lock and, for each one whose last
touch is too old, escalates: a `miss` counter, then a `mega_miss` counter, then
configured kill and multikill actions — the default action aborts the process, on
the theory that a deadlocked proxy is worse than a crashed one.
[`createWatchDog`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/guarddog_impl.cc#L191)
sets the petting interval to half the guard dog's own scan interval. There are
two guard dogs, one for the main thread and one for the workers, so they can be
tuned separately; `InstanceImpl` supplies them via `maybeCreateGuardDog`.

## Global policy, per-worker enforcement

Two subsystems show the shared-nothing pattern in miniature.

The overload manager measures process-wide resources (heap, connection counts)
on the main thread and pushes the results outward.
[`OverloadManagerImpl::start`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/overload_manager_impl.cc#L570)
populates a thread-local state object and arms a refresh timer; each tick reads
the monitors and then
[`flushResourceUpdates`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/overload_manager_impl.cc#L721)
batches the changes into one `runOnAllThreads` call plus per-dispatcher posts for
registered action callbacks. Workers only ever read their local copy. The actions
a worker registers for in the `WorkerImpl` constructor include
[`stopAcceptingConnectionsCb`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/worker_impl.cc#L194),
which disables that worker's listeners. A second, always-present
[`NullOverloadManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/null_overload_manager.h#L16)
— an implementation that is never overloaded — is handed to each worker
alongside the real one, and the connection handler picks it for any listener
whose config sets `shouldBypassOverloadManager()`, which is how the admin
interface stays reachable while the proxy is shedding load.

Overload *actions* are pushed state: the main thread computes them and a worker
reacts when its copy changes. *Load shed points* are the opposite, and they are
pull-based. A [`LoadShedPoint`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/overload/load_shed_point.h#L66)
has exactly one method, `shouldShedLoad()`, and code sitting at a named place in
the connection or request lifecycle calls it to ask whether to abandon the work
in front of it. The names are a fixed registry,
[`LoadShedPointNameValues`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/overload/load_shed_point.h#L12),
with entries for the accept path, HCM header decoding and codec creation, HTTP/1,
HTTP/2 and HTTP/3 codec dispatch, and connection pool creation among others; a
component resolves its own point once up front — in its constructor, or for a
listener when the connection handler adds it — through
[`OverloadManagerImpl::getLoadShedPoint`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/overload_manager_impl.cc#L660),
which returns `nullptr` for a point nobody configured.

The accept-path one is the clearest example.
[`TcpListenerImpl::configureLoadShedPoints`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/tcp_listener_impl.cc#L194)
caches `envoy.load_shed_points.tcp_listener_accept`, and the accept loop in
[`onSocketEvent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/network/tcp_listener_impl.cc#L63)
closes the socket it just accepted when the point fires (see
[Chapter 4](./04-accept-path.md)). Cheapness is the point of the pull model:
[`LoadShedPointImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/overload_manager_impl.h#L69)
keeps a `std::atomic<float>` probability that the main thread updates from the
same resource tick as the actions, so a probe on a worker is one atomic load and,
below saturation, one random draw — no thread-local slot and no post. Do not
confuse either mechanism with a listener's `reject_fraction_`, which is ordinary
pushed action state: the `RejectIncomingConnections` action calls
[`WorkerImpl::rejectIncomingConnectionsCb`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/worker_impl.cc#L202),
which sets a per-worker fraction on every listener that is not bypassing the
overload manager. The accept loop checks both, one line apart.

The drain manager decides whether a connection should be closed to let it be
re-established elsewhere.
[`DrainManagerImpl::startDrainSequence`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/drain_manager_impl.cc#L147)
sets an atomic flag and a deadline; workers then call
[`drainClose`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/drain_manager_impl.cc#L44)
which, under the default gradual strategy, returns true with probability equal to
the fraction of the drain window that has elapsed — spreading reconnections out
instead of stampeding. There is a server-level manager, created by
[`ProdComponentFactory::createDrainManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/exe/stripped_main_base.cc#L31)
with drain type `MODIFY_ONLY`, and a separate manager per listener, created by
[`ProdListenerComponentFactory::createDrainManager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_manager_impl.cc#L401)
with that listener's own drain type; a listener drain-closes if
[either one says so](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/listener_impl.h#L151).

## Hot restart, and why some of this looks strange

Several of the oddities above exist because a running Envoy must be able to hand
its listening sockets to a *new* Envoy process and then exit, with no dropped
connections and no lost stats. That is
[`HotRestart`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/hot_restart.h#L31), and it is why the
restarter is constructed before anything else.

The mechanism is a shared memory region plus a Unix domain socket keyed by a
"base id."
[`attachSharedMemory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/hot_restart_impl.cc#L26)
maps the region, and on epoch 0 initializes two `PTHREAD_PROCESS_SHARED`, robust
mutexes in it — this is why the application log and access log take a lock that
comes from the hot restarter rather than a plain `absl::Mutex`: the two processes
are writing the same files concurrently. Version and size checks on that region
are release asserts, because a mismatched layout would be silent corruption.

Every process is simultaneously a potential child and a potential parent, so
`HotRestartImpl` holds both a `HotRestartingChild` and a `HotRestartingParent`.
The child asks the parent for its listening file descriptors, one per worker when
`reuse_port` is in play, in
[`duplicateParentListenSocket`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/hot_restarting_child.cc#L127);
the parent answers on its main-thread dispatcher in
[`onSocketEvent`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/hot_restarting_parent.cc#L64).
When the child's workers are up, `startWorkers` calls
[`drainParentListeners`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/hot_restart_impl.cc#L113)
and then
[`startParentShutdownSequence`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/drain_manager_impl.cc#L218),
which arms a timer that eventually sends
[`sendParentTerminateRequest`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/hot_restarting_child.cc#L225).
Before that, the child pulls the parent's counters and gauges and merges them, so
that a hot restart does not look like a counter reset to a monitoring system (see
[Chapter 13](./13-observability-and-operations.md)).

```
  parent (epoch N)                  child (epoch N+1)
  ----------------                  -----------------
  listening, serving   <--- pass_listen_socket ---   startup
                       ---- fd ------------------>   bind/listen
                                                     workers start
  stop accepting       <--- drain_listeners -----
  draining connections <--- stats (merged) ------
  exit                 <--- terminate ----------    (after parent_shutdown_time)
```

The `SHMEM_FLAGS_INITIALIZING` bit in the shared region guards against a third
process starting while the second is still initializing, and every
`HotRestartImpl` constructor sets `PR_SET_PDEATHSIG` so that a child dies with
its parent rather than orphaning itself.
Small details, but they are the reason hot restart is a property of the process
model rather than a feature bolted onto it.
