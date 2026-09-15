package envoytest

import (
	"encoding/json"
	"fmt"

	bootstrapv3 "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	"google.golang.org/protobuf/encoding/protojson"

	// Linking the extension types scenarios use, so protojson can resolve the
	// google.protobuf.Any bodies in a bootstrap during validation. Registration
	// is global and process-wide, so the set internal/xds already imports comes
	// along with the xds import below; these are the extras that only appear in
	// test configs.
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/file/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	_ "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"

	_ "github.com/boweidu/envoy-view/internal/xds"
)

// AdminPort is the admin port inside the container. The framework owns it:
// scenarios that set their own admin block would break port discovery, so the
// block is overwritten rather than merged.
const AdminPort = 9901

// ConfigDir is where scenario files land inside the container. It already
// exists in the official image, which matters because files are copied in
// before the container has ever run and so cannot mkdir for themselves.
const ConfigDir = "/etc/envoy"

// BootstrapFile is the generated bootstrap's name within ConfigDir.
const BootstrapFile = "bootstrap.json"

// Config declares one Envoy under test.
type Config struct {
	// Name identifies the scenario in container names and failure messages.
	Name string

	// Bootstrap is the Envoy bootstrap as JSON, minus the admin block. Empty
	// means a bootstrap with nothing but the injected admin and node, which is
	// a valid Envoy that serves only the admin interface.
	//
	// JSON rather than YAML because protojson can validate it against the
	// Bootstrap message without pulling in a YAML dependency, which turns a
	// misspelled field into a clear error here instead of an opaque container
	// exit later.
	Bootstrap string

	// Files are extra files to place in ConfigDir, keyed by base name. This is
	// how filesystem xDS scenarios supply their discovery responses.
	Files map[string]string

	// LogLevel is Envoy's --log-level. Defaults to "info"; "debug" is worth it
	// when a scenario fails for reasons the config dump does not explain.
	LogLevel string

	// Ports are container ports to publish in addition to the admin port, for
	// scenarios that send real traffic through the proxy.
	Ports []uint32

	// AllowUnknownExtensions skips protojson validation of the bootstrap.
	//
	// Needed by the scenarios that exist precisely to feed envoy-view an
	// extension type it was not compiled against: validation would reject the
	// unresolvable Any before Envoy ever saw it, and Envoy accepting it is the
	// thing under test.
	AllowUnknownExtensions bool
}

// nodeDefaults identify the test Envoy in its own config dump.
var nodeDefaults = map[string]any{
	"id":      "envoy-view-test",
	"cluster": "envoy-view-test",
}

// render produces the bootstrap JSON to write into the container.
func (c Config) render() ([]byte, error) {
	doc := map[string]any{}
	if c.Bootstrap != "" {
		if err := json.Unmarshal([]byte(c.Bootstrap), &doc); err != nil {
			return nil, fmt.Errorf("scenario %q: bootstrap is not valid JSON: %w", c.Name, err)
		}
	}

	doc["admin"] = adminBlock(AdminPort)
	if _, ok := doc["node"]; !ok {
		doc["node"] = nodeDefaults
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("scenario %q: encode bootstrap: %w", c.Name, err)
	}

	if !c.AllowUnknownExtensions {
		if err := ValidateBootstrap(out); err != nil {
			return nil, fmt.Errorf("scenario %q: %w", c.Name, err)
		}
	}
	return out, nil
}

// adminBlock binds the admin interface to all interfaces inside the container,
// which is what makes it reachable through a published port. This is safe here
// and nowhere else: the container publishes only to 127.0.0.1 on the host.
func adminBlock(port uint32) map[string]any {
	return map[string]any{
		"address": map[string]any{
			"socket_address": map[string]any{
				"address":    "0.0.0.0",
				"port_value": port,
			},
		},
	}
}

// ValidateBootstrap checks that raw decodes as an Envoy Bootstrap and satisfies
// the protoc-gen-validate constraints Envoy itself applies.
//
// This catches the common authoring mistakes -- a misspelled field, a string
// where a number belongs, a missing required child -- in the test process,
// where the error names the field, rather than in a container whose only
// symptom is exit code 1.
func ValidateBootstrap(raw []byte) error {
	var bs bootstrapv3.Bootstrap
	// DiscardUnknown stays false: an unknown field is almost always a typo,
	// and silently dropping it would produce an Envoy that ignores the very
	// thing the scenario meant to configure.
	if err := (protojson.UnmarshalOptions{}).Unmarshal(raw, &bs); err != nil {
		return fmt.Errorf("bootstrap does not decode as envoy.config.bootstrap.v3.Bootstrap: %w", err)
	}
	if err := bs.ValidateAll(); err != nil {
		return fmt.Errorf("bootstrap fails Envoy's own validation: %w", err)
	}
	return nil
}
