# 3. The Extension and Factory Framework

*How does a protobuf type URL in a config file end up selecting a C++ class, and
what is that class allowed to hold on to?*

Envoy's source tree has a peculiar shape. There is a top-level directory,
`envoy/`, that contains almost nothing but abstract classes, and a second
top-level directory, `source/`, that contains all the code. The split is
deliberate and it is the single most important structural fact about the project:
[REPO_LAYOUT.md](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/REPO_LAYOUT.md) describes `envoy/` as "public" interface headers that are
"almost entirely 100% abstract classes", while `source/common/` holds core code and
`source/extensions/` holds the implementations that can be compiled in or out.

The reason for the split is that Envoy has no privileged implementations. An HTTP filter
written by a maintainer and one written by a vendor reach the data plane through exactly the
same door: a named, protobuf-configured factory looked up in a static registry. Almost every
pluggable concept in the proxy — access loggers, tracers, transport sockets, load balancing
policies, resource monitors, retry predicates, stat sinks, clusters, network and HTTP filters
— is registered this way. A handful of things — the HTTP connection manager and the
functionality around it — are treated as "core" because, in REPO_LAYOUT.md's words, they are
"so fundamental to Envoy that they will likely never be optional from a compilation
perspective"; even those reach the data plane through the registry like everything else.

## Factories, categories and the registry

An extension's entry point is not the extension class; it is a *factory* for it. The base of
the hierarchy is [`Config::UntypedFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/config/typed_config.h#L14),
which demands two strings: a `name()` (a reverse-DNS identifier such as
`envoy.filters.http.buffer`) and a `category()` (such as `envoy.filters.http`). Nearly all real
factories derive from [`Config::TypedFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/config/typed_config.h#L41),
which adds the ability to mint an empty instance of the factory's configuration protobuf:

```cpp
  std::set<std::string> configTypes() override {
    auto ptr = createEmptyConfigProto();
    ASSERT(ptr != nullptr);
    Protobuf::ReflectableMessage reflectable_message = createReflectableMessage(*ptr);
    return {std::string(reflectable_message->GetDescriptor()->full_name())};
  }
```

That method is the hinge of the whole design. Because a factory can produce an empty message
of its own config type, the framework can ask reflection for that message's fully-qualified
name, and thereby build a map from protobuf type name to factory without the factory having to
declare the mapping by hand.

The registry itself is
[`Registry::FactoryRegistry<Base>`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/registry/registry.h#L166), a class
template with no instances — everything is static. There is one registry per factory base
class, so `FactoryRegistry<NamedHttpFilterConfigFactory>` and
`FactoryRegistry<NamedNetworkFilterConfigFactory>` are separate namespaces of names that cannot
collide. It holds a name-to-factory map, a lazily built type-to-factory map produced by
`buildFactoriesByType()`, and a map of deprecated aliases; `registerFactory` throws on a
duplicate name and the type map's construction `RELEASE_ASSERT`s on a duplicate config type.

Registration happens through
[`Registry::RegisterFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/registry/registry.h#L508), whose constructor
does the work, wrapped in the `REGISTER_FACTORY` macro. A translation unit that implements an
extension ends with a line like `REGISTER_FACTORY(ApiKeyAuthFilterFactory,
Server::Configuration::NamedHttpFilterConfigFactory);` (or `LEGACY_REGISTER_FACTORY`, which
additionally registers a deprecated name, as the buffer filter does). When
`ENVOY_STATIC_EXTENSION_REGISTRATION` is defined this expands to a file-scope object whose
constructor runs before `main`; otherwise it expands to a `forceRegister...()` function that a
wrapper must call. Either way, linking the object file
into the binary is what makes the extension exist — which is why every extension library is
declared with `alwayslink = 1` by the `envoy_cc_extension` rule.

Alongside the per-type registries there is a second, weakly typed index:
[`FactoryCategoryRegistry`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/registry/registry.h#L104) maps a
category string to a [`FactoryRegistryProxy`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/registry/registry.h#L34)
for the corresponding registry. `RegisterFactory`'s constructor populates it automatically from
`instance_.category()`. This is how the server can enumerate everything it was built with:
[`InstanceBase::initializeOrThrow`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/server.cc#L464)
logs "statically linked extensions:" by walking the category registry, and a second walk over
the same registry later in that function fills in the `extensions` field of the node in the
bootstrap so a management server can see what this Envoy supports. It is also how `--disable-extensions` works:
[`OptionsImplBase::disableExtensions`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/options_impl_base.cc#L57)
splits each `category/name` pair and nulls out the factory pointer.

Not everything is meant to be operator-visible. Singleton registrations use
[`RegisterInternalFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/registry/registry.h#L608), which skips
the category index.

## From an `Any` to a factory

Envoy configuration never names a C++ class. A filter entry carries a `name` and a
`typed_config` of type `google.protobuf.Any`, and the type URL inside that `Any` is what
selects the implementation. The resolution logic lives in
[`Config::Utility`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/utility.h#L68):

```
typed_config (Any)
   │  type_url: "type.googleapis.com/envoy...v3.Buffer"
   ▼
getFactoryType()  ──► strips the URL prefix; if the payload is an
   │                  xds.type.v3.TypedStruct, uses its inner type_url
   ▼
FactoryRegistry<Base>::getFactoryByType("envoy...v3.Buffer")
   │
   ▼
factory ──► createEmptyConfigProto() ──► translateOpaqueConfig() ──► typed config message
```

[`getFactoryType`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/utility.h#L290) reduces the URL to a
descriptor name, transparently unwrapping (one level; nested structs are not handled) the
`TypedStruct` wrappers that let a control plane
send JSON for a type the control plane does not have compiled in.
[`getAndCheckFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/utility.h#L266) then looks the
type up and throws a "Didn't find a registered implementation" exception unless the config
marked the extension optional. Finally
[`translateToFactoryConfig`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/utility.h#L334) asks
the factory for an empty message and hands both to
[`Utility::translateOpaqueConfig`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/config/utility.cc#L292),
which either unpacks the `Any` directly or, for a `TypedStruct`, converts the JSON. The
`name` field survives only as a label — for logging, for per-route configuration overrides, and
for enabling or disabling the filter on a route.

## One HTTP filter, end to end

Take a config fragment naming `envoy.filters.http.buffer`. The HTTP connection manager's
configuration object constructs a
[`FilterChainHelper`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.h#L73)
parameterised on the context type and the factory base type, then calls
[`processFilters`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.h#L88) over the
repeated `http_filters` field. For each entry,
[`processFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.h#L96) either delegates
to ECDS (see [Chapter 2](./02-configuration-and-xds.md)) or performs the static path described
above and invokes:

```cpp
  virtual absl::StatusOr<Http::FilterFactoryCb>
  createFilterFactoryFromProto(const Protobuf::Message& config, const std::string& stat_prefix,
                               Server::Configuration::FactoryContext& context) PURE;
```

declared on
[`NamedHttpFilterConfigFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/filter_config.h#L310).
Note the two-stage shape of the result. The factory is called once, at configuration time, and
returns a [`Http::FilterFactoryCb`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter_factory.h#L24) — a
`std::function` that will be called once per stream to push filter instances into a
[`FilterChainFactoryCallbacks`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/http/filter.h#L1351). Expensive
parsing and validation happen in the first stage; the per-stream stage is a lambda holding a
shared pointer to immutable config. The buffer filter is a compact example:
[`BufferFilterFactory::createFilterFactoryFromProtoTyped`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/buffer/config.cc#L18)
builds one `BufferFilterConfig` and captures it in the returned lambda. Most filters never
write that boilerplate themselves; they derive from
[`FactoryBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/common/factory_base.h#L63) or
[`DualFactoryBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/common/factory_base.h#L160),
which implement `createEmptyConfigProto`, downcast-and-validate the message, and forward to a
strongly typed override.

`processFilter` does three more things with the factory before moving on. It records the
factory's declared dependencies. It asks `isTerminalFilterByProto` and passes the answer to
`validateTerminalFilters`, so a router placed in the middle of a chain is rejected at config
time rather than at request time. And it wraps the callback in a static filter config provider,
so that static and dynamic (ECDS) filters present one interface to the runtime. At stream
time, [`FilterChainUtility::createFilterChainForFactories`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.cc#L18)
walks the resulting list, skipping disabled filters and substituting a
[`MissingConfigFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/filter_chain_helper.h#L29) that
returns a 500 if a dynamic config never arrived; what the resulting chain then does with a
request is [Chapter 6](./06-http-connection-manager.md)'s subject.

## What a factory is allowed to touch

A factory needs server resources, but which ones it may legitimately keep a reference to
depends on how long the object it creates will live. Envoy encodes that in a small hierarchy of
context interfaces in `envoy/server/factory_context.h`.

[`CommonFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L54) is the broad
base: options, the main-thread dispatcher, the `Api`, runtime, the singleton manager, thread
local storage, the cluster manager, stats scopes, the time source, validation visitors.
[`ServerFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L162) extends it
with things whose lifetime is explicitly the server's — the HTTP, gRPC and router contexts, the
bootstrap proto, the overload manager, the SSL context manager, the secret manager — and the
contract is that a reference to it is safe forever.

[`GenericFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L250) is the
narrow interface most extensions actually see: a `ServerFactoryContext&`, plus a validation
visitor, an `Init::Manager` and a `Stats::Scope` that belong to whatever owns this context —
server, listener or cluster — and are therefore *not* guaranteed to outlive it.
[`FactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L290) adds listener-flavoured
facts: a drain decision, traffic direction, whether the listener is QUIC, a prefixed stats
scope. [`ListenerFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L355)
adds the listener scope and `ListenerInfo`, and
[`FilterChainFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L327)
adds `startDraining()`. Upstream HTTP filters get a separate, deliberately smaller
[`UpstreamFactoryContext`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/server/factory_context.h#L376) with only
a server context, an init manager and a cluster scope.

The implementations are thin.
[`FactoryContextImplBase`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/factory_context_impl.h#L11)
holds a `Server::Instance&` and two stats scopes and forwards nearly everything;
[`FactoryContextImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/factory_context_impl.h#L43) adds the
init manager and drain decision. When code needs to construct a context out of parts — a
server context plus somebody else's scope and init manager — it uses
[`GenericFactoryContextImpl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/generic_factory_context.h#L8),
whose constructors in
[`generic_factory_context.cc`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/server/generic_factory_context.cc) either default the
scope and init manager to the server's or copy them from an existing generic context.

## The generic matching framework

Several parts of the proxy need to answer the same question — given some object, which of a
configured set of outcomes applies? — about different objects: a connection socket when picking
a filter chain (see [Chapter 4](./04-accept-path.md)), an HTTP request when picking a route (see
[Chapter 7](./07-routing.md)). Rather than one ad-hoc predicate language per site, there is one
framework, parameterised on the type being matched, and every piece of it is a registered
extension of the kind described above.

The core abstraction is [`MatchTree<DataType>`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L201), which is
handed a `DataType` and returns an
[`ActionMatchResult`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L166). Its nodes are assembled
from three pluggable pieces. A [`DataInput<DataType>`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L411)
pulls one value out of the object — a header, the SNI, a filter state key — and an
[`InputMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L252) decides whether that value is a match;
[`SingleFieldMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/field_matcher.h#L124) is the pair
of them, and [`AllFieldMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/field_matcher.h#L30) and
its `Any`/`Not` siblings compose those into boolean predicates. On a match the leaf yields an
[`Action`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L104) — a typed object built at config time that the
caller downcasts with `getTyped<T>()`, which is how a route or a filter chain gets attached to a
match. Each piece has its own factory category: `envoy.matching.action`,
`envoy.matching.input_matchers`, `envoy.matching.common_inputs`, and a per-data-type
`envoy.matching.<type>.input`.

Two node shapes cover the configuration surface (an unset `matcher_type` produces a third,
`AnyMatcher`, which just runs its `on_no_match`):
[`ExactMapMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/exact_map_matcher.h#L12) and its
[prefix sibling](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/prefix_map_matcher.h#L30), which extract
one input and look it up — in a hash map for the exact case, in a radix tree walked
longest-prefix-first for the other — and
[`ListMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/list_matcher.h#L14), which evaluates predicates
in order and takes the first hit.
[`MatchTreeFactory`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/matcher/matcher.h#L130) builds them
recursively from the protobuf, resolving every input, matcher and action through
`Config::Utility` exactly as above; an `on_match` may nest a whole subtree instead of an action.

The result is three-valued, not two. `ActionMatchResult` is the tree-level form, holding an
action or one of the two non-match states; the enum the field matchers and input matchers speak
is [`MatchResult`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L76):

```cpp
enum class MatchResult {
  // The match comparison was completed, and there was no match.
  NoMatch,
  // The match comparison was completed, and there was a match.
  Matched,
  // The match could not be completed, e.g. due to the required data
  // not being available.
  InsufficientData,
};
```

That third state is the interesting one. A
[`DataInputGetResult`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L249) carries a
[`DataAvailability`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/matcher/matcher.h#L291) beside its value, and a field
matcher that fails to match while more data might still arrive reports insufficient data rather
than "no" — so an undecided tree can be evaluated early and re-evaluated later. It is the same
shape of "cannot decide yet, ask me again" contract that the listener filter peek loop expresses
at the socket level with `StopIteration` and `maxReadBytes()` (see
[Chapter 4](./04-accept-path.md)), though the two mechanisms are unrelated. Callers that know
everything is already present assert `isComplete()`: filter chain matching does exactly that in
[`findFilterChainUsingMatcher`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/listener_manager/filter_chain_manager_impl.cc#L586),
since the listener filters have already run by the time it is called.

## Giving an arbitrary filter a matcher

Any HTTP filter can be put behind a match tree without knowing about matching, by wrapping its
config in an `ExtensionWithMatcher` message and letting
[`MatchDelegateConfig`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/match_delegate/config.h#L150)
(`envoy.filters.http.match_delegate`) build the inner filter and the tree together. The result is
a [`DelegatingStreamFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/match_delegate/config.h#L22)
that forwards every callback to the wrapped filter, except when the tree resolves to
[`SkipAction`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/filters/http/match_delegate/config.h#L19), in which
case the filter is bypassed for that stream (as it is when no tree is configured at all); any
other action is handed to the wrapped filter via `onMatchCallback`. It is also the clearest demonstration of the
three-state result: the delegate records the match as settled only when the result
`isComplete()`, so a tree that could not decide on request headers is tried again on trailers and
on the response path.

## Choosing extensions at build time

Which extensions exist in a given binary is a Bazel decision.
[`source/extensions/extensions_build_config.bzl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/extensions_build_config.bzl)
maps each extension's canonical name to its `:config` target; a handful of extensions that the
core genuinely cannot run without — the XFF original-IP detector, the UUID request-ID
generator, the round-robin load balancer — are listed instead in `_required_extensions` inside
[`all_extensions.bzl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/all_extensions.bzl). `envoy_all_extensions()`
unions the two and returns the `_envoy_extension` alias of each target.

That alias is what makes removal possible. `envoy_cc_extension` in
[`bazel/envoy_library.bzl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/bazel/envoy_library.bzl) declares both the real library and a
shim named `<name>_envoy_extension` whose dependency is `select()`ed on an `is_enabled` config
setting, which `envoy_extension_package` in
[`bazel/envoy_build_system.bzl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/bazel/envoy_build_system.bzl) derives from a per-package
boolean flag. Deselecting an extension makes the shim empty, the object file never links, the
static initializer never runs, and the name simply is not in the registry. Site-specific builds
override the whole map by pointing the `@envoy_build_config` repository at their own copy of
`extensions_build_config.bzl`, as described in [bazel/README.md](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/bazel/README.md).

Extensions that are not held to the core bar live in `contrib/` with a parallel
[`contrib_build_config.bzl`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/contrib/contrib_build_config.bzl); they are excluded from the
default images and require an end-user sponsor.
[EXTENSION_POLICY.md](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/EXTENSION_POLICY.md) also requires every extension to declare a
`status` (`stable`, `alpha`, `wip`) and a `security_posture` in
[`extensions_metadata.yaml`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/extensions/extensions_metadata.yaml); the security
posture is what determines whether a bug in that extension is treated as a security release.

## Two things factories lean on

Extensions frequently need process-wide state — a shared cache, a config provider manager — and
must not each construct their own. The
[`Singleton::Manager`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/singleton/manager.h#L62), reachable from any factory
context, stores singletons by name, normally as a `weak_ptr` so an unused one can be collected,
or as a pinned `shared_ptr` when the caller passes `pin`. Names are themselves registered
extensions: `SINGLETON_MANAGER_REGISTRATION(foo)` declares a
[`Singleton::Registration`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/envoy/singleton/manager.h#L18) factory, and
[`ManagerImpl::get`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/singleton/manager_impl.cc#L11) `ENVOY_BUG`s
if the name was never registered, and asserts that it is called on the main thread. The HTTP
filter chain code uses this for its filter config provider managers.

Second, filters can declare what they need from other filters. A factory may override
`dependencies()` to say that it requires or provides a named header, filter-state key or
dynamic metadata entry;
`processFilter` feeds those declarations to
[`DependencyManager::registerFilter`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/dependency_manager.h#L23)
in decode order, and
[`validDecodeDependencies`](https://github.com/envoyproxy/envoy/blob/981d3923fed302ab44ee9fd265fb078d4aabbd85/source/common/http/dependency_manager.cc#L11)
walks the chain accumulating what has been provided, returning a `NotFoundError` naming the
first filter whose requirement is unmet. Only the decode path is checked today.
