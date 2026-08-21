# Summary

[How Envoy Works](./README.md)

# Part I — The Map

- [What Envoy Is: Process and Threading Model](./01-process-model.md)
- [Configuration and xDS](./02-configuration-and-xds.md)
- [The Extension and Factory Framework](./03-extension-framework.md)

# Part II — The Journey of a Request

- [The Accept Path: Listeners, Sockets and Transport](./04-accept-path.md)
- [Network Filters and the HTTP Codecs](./05-network-filters-and-codecs.md)
- [The HTTP Connection Manager and the Filter Chain](./06-http-connection-manager.md)
- [Routing: From Headers to a Cluster](./07-routing.md)
- [Upstream: Clusters, Load Balancing and Connection Pools](./08-upstream.md)
- [The Response, Retries, Timeouts and Teardown](./09-response-and-teardown.md)
- [One Request, End to End](./10-one-request-end-to-end.md)

# Part III — The Machine Room

- [The Event Loop, Threading and Object Lifetime](./11-event-loop-and-threading.md)
- [Buffers, Flow Control and I/O](./12-buffers-and-io.md)
- [Stats, Logging, Tracing, Runtime and the Admin Interface](./13-observability-and-operations.md)
- [Requests Envoy Makes for Itself](./14-async-client.md)
