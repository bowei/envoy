package xds

import (
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	// Registering the extension types we traverse. Importing a generated
	// package is what puts its messages into protoregistry.GlobalTypes, which
	// is what lets protojson decode the matching google.protobuf.Any bodies.
	//
	// This list only needs to cover types the graph builder walks into. Every
	// other extension is handled by the placeholder fallback below and is still
	// displayed from the raw JSON, so adding an import here is only necessary
	// when a new type gains real traversal support.
	_ "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
)

// resolver resolves Any type URLs for protojson, substituting an empty
// placeholder message for types that are not linked into this binary.
//
// This matters more than it looks. protojson fails the entire unmarshal if it
// cannot resolve a single nested Any, and a real config dump is full of
// extension points (HTTP filters, transport sockets, access loggers, custom
// filters) whose types depend on which Envoy build produced the dump and which
// go-control-plane version we compiled against. Without a fallback, one unknown
// filter makes the whole listener undecodable.
//
// The placeholder has no fields, so its body is dropped during unmarshal. That
// is acceptable because raw JSON is retained separately for display; the typed
// message exists only so the graph builder can walk references.
type resolver struct {
	base *protoregistry.Types

	mu           sync.Mutex
	placeholders map[protoreflect.FullName]protoreflect.MessageType
}

func newResolver() *resolver {
	return &resolver{
		base:         protoregistry.GlobalTypes,
		placeholders: make(map[protoreflect.FullName]protoreflect.MessageType),
	}
}

func (r *resolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if mt, err := r.base.FindMessageByName(name); err == nil {
		return mt, nil
	}
	return r.placeholder(name)
}

func (r *resolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	if mt, err := r.base.FindMessageByURL(url); err == nil {
		return mt, nil
	}
	return r.placeholder(messageNameFromURL(url))
}

func (r *resolver) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
	return r.base.FindExtensionByName(field)
}

func (r *resolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return r.base.FindExtensionByNumber(message, field)
}

// placeholder builds (and caches) an empty message type named name.
func (r *resolver) placeholder(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if !name.IsValid() {
		name = unknownPlaceholderName
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if mt, ok := r.placeholders[name]; ok {
		return mt, nil
	}

	pkg, msg := splitFullName(name)
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        strPtr("envoyview/placeholder/" + string(name) + ".proto"),
		Syntax:      strPtr("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: strPtr(msg)}},
	}
	if pkg != "" {
		fdp.Package = strPtr(pkg)
	}
	// Placeholders are deliberately not registered in any shared registry: two
	// dumps parsed concurrently must not race to define the same name, and a
	// placeholder must never shadow a real type that a later import provides.
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		return nil, err
	}
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))
	r.placeholders[name] = mt
	return mt, nil
}

const unknownPlaceholderName protoreflect.FullName = "envoyview.placeholder.Unknown"

// messageNameFromURL strips the leading type host from an Any type URL:
// "type.googleapis.com/envoy.config.listener.v3.Listener" -> the message name.
func messageNameFromURL(url string) protoreflect.FullName {
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		url = url[i+1:]
	}
	return protoreflect.FullName(url)
}

func splitFullName(name protoreflect.FullName) (pkg, msg string) {
	s := string(name)
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func strPtr(s string) *string { return &s }
