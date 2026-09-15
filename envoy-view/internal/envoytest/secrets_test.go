package envoytest_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"

	"github.com/boweidu/envoy-view/internal/envoytest"
	"github.com/boweidu/envoy-view/internal/graph"
	"github.com/boweidu/envoy-view/internal/xds"
)

// Throwaway self-signed material, generated once with
//
//	openssl req -x509 -newkey rsa:2048 -nodes -keyout k.pem -out c.pem \
//	    -days 36500 -subj "/CN=test"
//
// and pasted here so a scenario needs no setup step and no clock-sensitive
// expiry. It is a local container's key and guards nothing, which is what makes
// it safe to check in: these tests exist to pin down exactly how much of it
// reaches a config dump.
const testCertPEM = `-----BEGIN CERTIFICATE-----
MIICnDCCAYQCCQCr6LaBixk9RjANBgkqhkiG9w0BAQsFADAPMQ0wCwYDVQQDDAR0
ZXN0MCAXDTI2MDkxNTE3Mzg0MloYDzIxMjYwODIyMTczODQyWjAPMQ0wCwYDVQQD
DAR0ZXN0MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAujn1ppkAoZtj
glNd9F8or0WP0hl//ms9ls2fDNJ860eBV4V9kFCMMzVb/M4b1TLfwC87gU34NaUk
DhbjzZHWylmHia06tfKquwCGPK14TF2stoY4zNrbLymNqiKsgMc2jtIbrngaeoiF
Wb3W9JrSmHgX5cDgkveDX5j21cEWscjC5h+3YaWYPhUfweNR2lMhPesLKDaYKSrr
sA1sEQAIKuzmsdKHCsMhRrxcoy5hAGH/h8tmZCJRroJ0eOC7Q6d3x1tRX426nknU
bZCNTlY51ZeyXlmhRTxhj7oDdA34yUXbGWd66wqvBiRmDSm7mz7/yKNFMymmIarO
zl0s/6/WcwIDAQABMA0GCSqGSIb3DQEBCwUAA4IBAQBMayPFT8giZ36HWjmGc1wR
xACyRh1mfmFnUNoBTLU8aZFu+E3+g3yAcwMxOJlLoa/NaEI5PxXAvtu7gPkVCFqx
91mPAhzO1gYzW3wyJSGolElTV8QmNY2z3WLy/NYiDM6L7gAFxxbGnmex8fPULJVG
AATXVUsagXf7lKs4iUBlqrfYB6LQfr5Q1UtPv/o9Pgp4Y+oI5++DKHGVo6ZJtGL6
hmi0rNFX0KQg2a0Zz+QzruCBDJvxk8OX4x2ZqX4CJG81KGzTkzKr92X8xDRc4Ze6
3P9wuJbAPf21aSST+WoF3Inw8+MoFL8WWEdMOmJizE2ura0htoFNQSJ5h7AixdfN
-----END CERTIFICATE-----
`

const testKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC6OfWmmQChm2OC
U130XyivRY/SGX/+az2WzZ8M0nzrR4FXhX2QUIwzNVv8zhvVMt/ALzuBTfg1pSQO
FuPNkdbKWYeJrTq18qq7AIY8rXhMXay2hjjM2tsvKY2qIqyAxzaO0huueBp6iIVZ
vdb0mtKYeBflwOCS94NfmPbVwRaxyMLmH7dhpZg+FR/B41HaUyE96wsoNpgpKuuw
DWwRAAgq7Oax0ocKwyFGvFyjLmEAYf+Hy2ZkIlGugnR44LtDp3fHW1FfjbqeSdRt
kI1OVjnVl7JeWaFFPGGPugN0DfjJRdsZZ3rrCq8GJGYNKbubPv/Io0UzKaYhqs7O
XSz/r9ZzAgMBAAECggEAWqJKxEJC5GRUEeGxIHYPvv4D+SUf/hsDZpm8hukKkxfC
A26cpdgN4/5cPrWxJhoUe9yBAXWJD9LHsXPCexI3j1PzezYsFNF99nVS38Utfz04
Cb1Zd5osgs+eeudVPXe3PdtUTZ12hZxcCkkyjOmtBTetqcwtgFmmPqibuXy4Jt3S
EdCnLkwklTBSoisyviu9XBOQ2qH3p3Awz01hXN/quXApDnWcF6y1KO3VbF9XmBz1
d8KrnKTIM0hn5qteN0jN8qSVSoB9lrqahOGFpU95+zRMmkbTEpV3OkkZwmMqdcHO
BdJQz5m/BvPes0IgmTK6wLbXWw94ukJ8KKu1ZgJIwQKBgQDiUkxFVb++PGt+mxYv
e1BtkMMmg6Ew6mXbL6+3MjORFnshIkCKSpZ/hNV6TyWQ0lxRIn2iZU+AELD/4jmB
GCWwX6NIvtdoNU/76P+ZEksc7JNqjRMIdAVbTQobOPflLVy4GSG9Cmgqx7lpVbKH
sFIDgQ/qLW0x55vma+8nUSzzTwKBgQDSpad0Xc0sHmOD2je+1jGsnWrSvj6TJxP5
lT+WWmPfWAhzEFHCFfNiFNXhzACQCig+su5TDs7taxallk5SvZbtAPb1b82QkgD2
gea3cDQ2ULo/3fYqLKTOh4QmCSgUkJNjRuY1zc0ZEuzmcXi2fc734sFUEpZZDXi8
gS/RyYuxnQKBgG7KPtAKRA0KYszdeqTPxvV70ix+b2AUvrvnwir6BkhWKvxzWgjY
rofKLP7s08TrVYnaSoo+8gYNJbh9tAzAF2MZzkMEOUqoHnmA++6hB+gm35tfaBvR
P/YL5pCg+KlV5XexxdRWzdtzXg50hyrpY5yXh4Tpq/SsHqNT3wTuNgT9AoGBALEy
fK0dAy0r2xbdiKtWT68fNO9W7hindNwtOrJmE0GcMm9ouP4FrRlC4bDyBT8l7Dji
GC1ydYuu2/wrdnOP3Ng+SYCprkkBKSI0oDqLfsB6JFL5isxrha/eu8GrTjYOcI3A
5IM6Pl/rVbF8nskVB/fqnir0/9ilxnz8R5e4bXTZAoGATTR3dIzoQzWWWVuMfTPX
p1TYSoWUG1cd8gmIj7Utf1Hmr29CFcpY+EGy70jVwEAEE3EceLX41/ZohPWv2aBL
0dkj5cX3y4zXW6IlHEfTeKx7akTVJ59vSmQK0207FvOms4eEZEGLyxY5MGU6gAe1
TsQIbQckQwJxbv3MM/tyQP4=
-----END PRIVATE KEY-----
`

// jsonQuote embeds PEM, which is full of newlines, into a bootstrap literal.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// tlsCertificateSecret is a Secret carrying the throwaway keypair inline.
//
// This is the bare message, which is what static_resources.secrets takes; an
// SDS response carries the same thing inside an Any, so it goes through
// anySecret first.
func tlsCertificateSecret(name string) string {
	return fmt.Sprintf(`{
      "name": %s,
      "tls_certificate": {
        "certificate_chain": {"inline_string": %s},
        "private_key": {"inline_string": %s}
      }
    }`, jsonQuote(name), jsonQuote(testCertPEM), jsonQuote(testKeyPEM))
}

// tlsCertificateSecretBytes is the same keypair supplied as inline_bytes
// instead of inline_string. The two are interchangeable to Envoy but not to
// the redactor, which is the point of carrying both forms.
func tlsCertificateSecretBytes(name string) string {
	return fmt.Sprintf(`{
      "name": %s,
      "tls_certificate": {
        "certificate_chain": {"inline_bytes": %s},
        "private_key": {"inline_bytes": %s}
      }
    }`, jsonQuote(name),
		jsonQuote(base64.StdEncoding.EncodeToString([]byte(testCertPEM))),
		jsonQuote(base64.StdEncoding.EncodeToString([]byte(testKeyPEM))))
}

// validationContextSecret is the other half of a mutual-TLS pair: a trust
// bundle, which unlike a keypair holds nothing private.
func validationContextSecret(name string) string {
	return fmt.Sprintf(`{
      "name": %s,
      "validation_context": {"trusted_ca": {"inline_string": %s}}
    }`, jsonQuote(name), jsonQuote(testCertPEM))
}

// redacted is what Envoy substitutes for a field the proto marks sensitive.
const redacted = "[redacted]"

// TestSecretStaticIsRedactedInTheConfigDump is the security-relevant one.
//
// envoy-view shows a resource's raw JSON verbatim in its detail pane, so
// whatever Envoy puts in the dump is what ends up on someone's screen and in
// whatever bug report they paste it into. Private keys are configured inline
// here -- the worst case -- and the test records exactly how much of that
// survives into the dump.
//
// What survives: the certificate chain, verbatim. Only fields the proto marks
// sensitive are redacted, and a certificate is not secret; Envoy hands the same
// bytes to every client that connects. The private key is replaced, and the
// replacement keeps the DataSource oneof arm it was configured with, so the
// marker arrives as inline_string for one secret and as base64 inline_bytes for
// the other. Anything grepping a dump for leaked keys has to know both shapes.
//
// If a future Envoy ever stops redacting, this test fails and envoy-view has to
// mask secrets itself before it can display them.
func TestSecretStaticIsRedactedInTheConfigDump(t *testing.T) {
	bootstrap := fmt.Sprintf(`{
  "static_resources": {
    "secrets": [%s, %s]
  }
}`, tlsCertificateSecret("inline_string_cert"), tlsCertificateSecretBytes("inline_bytes_cert"))

	e := envoytest.Start(t, envoytest.Config{Name: "secret-static", Bootstrap: bootstrap})
	ix := e.Index()

	r := ix.Get(xds.KindSecret, "inline_string_cert")
	if r == nil {
		t.Fatalf("secret inline_string_cert missing from dump\n%s", ix.Raw)
	}
	if r.Kind != xds.KindSecret {
		t.Errorf("kind = %q, want %q", r.Kind, xds.KindSecret)
	}
	if r.State != xds.StateStatic {
		t.Errorf("state = %q, want %q", r.State, xds.StateStatic)
	}
	if r.VersionInfo != "" {
		t.Errorf("version_info = %q, want empty for a bootstrap secret", r.VersionInfo)
	}
	if r.TypeURL != "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret" {
		t.Errorf("type URL = %q", r.TypeURL)
	}
	if r.DecodeErr != nil {
		t.Fatalf("secret did not decode: %v", r.DecodeErr)
	}
	if len(ix.Warnings()) > 0 {
		t.Errorf("parsing a dump with secrets produced warnings: %v", ix.Warnings())
	}

	// The whole dump, not just the secret resource: the bootstrap is echoed
	// back in its own section with the same secrets in it, and envoy-view
	// serves that too. A key leaking there would be just as visible.
	dump := string(e.ConfigDump())
	for _, leak := range []string{"BEGIN PRIVATE KEY", pemBody(testKeyPEM)} {
		if strings.Contains(dump, leak) {
			t.Errorf("config dump leaks private key material (%q); envoy-view would "+
				"render it in the detail pane", truncate(leak, 40))
		}
	}
	// Base64 of the key, in case a future Envoy echoes an inline_bytes secret
	// back without redacting it -- the leak would not contain the PEM armour.
	if b64 := base64.StdEncoding.EncodeToString([]byte(testKeyPEM)); strings.Contains(dump, b64) {
		t.Error("config dump leaks the private key as base64 inline_bytes")
	}

	strSec := secretMessage(t, r)
	if got := strSec.GetTlsCertificate().GetPrivateKey().GetInlineString(); got != redacted {
		t.Errorf("private_key.inline_string = %q, want %q\n%s", got, redacted, r.Raw)
	}
	// Redaction does not move the value to the other oneof arm.
	if got := strSec.GetTlsCertificate().GetPrivateKey().GetInlineBytes(); len(got) != 0 {
		t.Errorf("private_key.inline_bytes = %q, want the marker in inline_string instead", got)
	}
	// Deliberately asserted, not tolerated: the certificate is public, and a
	// tool that hid it would be hiding the field people open a dump to check.
	if got := strSec.GetTlsCertificate().GetCertificateChain().GetInlineString(); got != testCertPEM {
		t.Errorf("certificate_chain.inline_string = %q, want the configured PEM back verbatim", got)
	}

	bytesSec := secretMessage(t, ix.Get(xds.KindSecret, "inline_bytes_cert"))
	if got := string(bytesSec.GetTlsCertificate().GetPrivateKey().GetInlineBytes()); got != redacted {
		t.Errorf("private_key.inline_bytes = %q, want %q", got, redacted)
	}
	if got := string(bytesSec.GetTlsCertificate().GetCertificateChain().GetInlineBytes()); got != testCertPEM {
		t.Errorf("certificate_chain.inline_bytes = %q, want the configured PEM back verbatim", got)
	}
}

func secretMessage(t *testing.T, r *xds.Resource) *tlsv3.Secret {
	t.Helper()
	if r == nil {
		t.Fatal("secret missing from dump")
	}
	sec, ok := r.Message.(*tlsv3.Secret)
	if !ok {
		t.Fatalf("secret %q decoded as %T, want *tlsv3.Secret", r.Name, r.Message)
	}
	return sec
}

// pemBody returns the first body line of a PEM block, which is distinctive
// enough to catch a leak that stripped the armour.
func pemBody(pem string) string {
	lines := strings.Split(strings.TrimSpace(pem), "\n")
	if len(lines) < 3 {
		panic("pemBody: not a PEM block")
	}
	return lines[1]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// tlsPipeline terminates TLS on the listener and originates it to the upstream,
// each side naming a static secret by SDS name with no sds_config -- the form
// that resolves against static_resources.secrets.
func tlsPipeline(certSecret, caSecret string) string {
	return fmt.Sprintf(`{
  "static_resources": {
    "secrets": [%s, %s],
    "listeners": [{
      "name": "ingress_tls",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "transport_socket": {
          "name": "envoy.transport_sockets.tls",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
            "common_tls_context": {
              "tls_certificate_sds_secret_configs": [{"name": %s}]
            }
          }
        },
        "filters": [{
          "name": "envoy.filters.network.tcp_proxy",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
            "stat_prefix": "tls",
            "cluster": "tls_backend"
          }
        }]
      }]
    }],
    "clusters": [{
      "name": "tls_backend",
      "type": "STATIC",
      "connect_timeout": "1s",
      "transport_socket": {
        "name": "envoy.transport_sockets.tls",
        "typed_config": {
          "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext",
          "common_tls_context": {
            "validation_context_sds_secret_config": {"name": %s}
          }
        }
      },
      "load_assignment": {
        "cluster_name": "tls_backend",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8443}
        }}}]}]
      }
    }]
  }
}`, tlsCertificateSecret(certSecret), validationContextSecret(caSecret),
		jsonQuote(certSecret), jsonQuote(caSecret))
}

// TestTLSTransportSocketsLinkToSecrets covers the reason secrets are in the
// graph at all: "which listener uses this cert" and "what does this cluster
// trust" are questions about edges, and the names live buried in a
// transport_socket's typed_config where nothing else surfaces them.
//
// Both directions are exercised in one Envoy, because the downstream and
// upstream contexts are different messages reached by different code paths in
// the builder.
func TestTLSTransportSocketsLinkToSecrets(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "tls-secret-edges",
		Bootstrap: tlsPipeline("server_cert", "upstream_ca"),
	})

	ix := e.Index()
	for _, name := range []string{"server_cert", "upstream_ca"} {
		if r := ix.Get(xds.KindSecret, name); r == nil {
			t.Fatalf("secret %q missing from dump\n%s", name, ix.Raw)
		} else if r.State != xds.StateStatic {
			t.Errorf("secret %q state = %q, want %q", name, r.State, xds.StateStatic)
		}
	}

	g := e.Graph(graph.Options{})
	if len(g.Problems) > 0 {
		t.Errorf("a config whose secrets all resolve reported problems: %+v", g.Problems)
	}

	for _, tc := range []struct {
		from graph.NodeID
		to   graph.NodeID
	}{
		// The downstream cert hangs off the filter chain that terminates TLS,
		// not off the listener: one listener can present different certs per
		// chain.
		{"listener/ingress_tls/fc/0", "secret/server_cert"},
		{"cluster/tls_backend", "secret/upstream_ca"},
	} {
		n := nodeByID(g, tc.to)
		if n == nil {
			t.Errorf("no node %q in the graph; nodes: %s", tc.to, nodeIDs(g))
			continue
		}
		if n.Kind != graph.NodeSecret {
			t.Errorf("node %q kind = %q, want %q", tc.to, n.Kind, graph.NodeSecret)
		}
		if n.Status != graph.StatusOK {
			t.Errorf("node %q status = %q, want %q (notes: %v)", tc.to, n.Status, graph.StatusOK, n.Notes)
		}
		if n.Resource == nil || n.Resource.Kind != xds.KindSecret {
			t.Errorf("node %q resource ref = %+v, want a secret ref", tc.to, n.Resource)
		}

		ed := edgeBetween(g, tc.from, tc.to)
		if ed == nil {
			t.Errorf("no edge %s -> %s; the UI cannot show who uses this secret", tc.from, tc.to)
			continue
		}
		if ed.Status != graph.StatusOK {
			t.Errorf("edge %s -> %s status = %q, want %q", tc.from, tc.to, ed.Status, graph.StatusOK)
		}
		if ed.Label != "sds" {
			t.Errorf("edge %s -> %s label = %q, want %q", tc.from, tc.to, ed.Label, "sds")
		}
	}
}

// sdsResponse wraps a Secret the way a filesystem SDS subscription expects.
func sdsResponse(version, secret string) string {
	return `{"version_info": "` + version + `", "resources": [` + anySecret(secret) + `]}`
}

// anySecret adds the @type key a DiscoveryResponse resource needs. The
// bootstrap form must not carry it: static_resources.secrets is a typed Secret
// field, and an "@type" in it is an unknown field Envoy rejects.
func anySecret(secret string) string {
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(secret), &fields); err != nil {
		panic(err)
	}
	fields["@type"] = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
	out, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// sdsListener terminates TLS with a cert fetched over SDS rather than one
// inlined in the bootstrap, which is how every real deployment does it.
func sdsListener(secretName, path string) string {
	return fmt.Sprintf(`{
  "static_resources": {
    "listeners": [{
      "name": "ingress_sds",
      "address": {"socket_address": {"address": "0.0.0.0", "port_value": 10000}},
      "filter_chains": [{
        "transport_socket": {
          "name": "envoy.transport_sockets.tls",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext",
            "common_tls_context": {
              "tls_certificate_sds_secret_configs": [{
                "name": %s,
                "sds_config": {
                  "path_config_source": {"path": %s},
                  "resource_api_version": "V3",
                  "initial_fetch_timeout": "5s"
                }
              }]
            }
          }
        },
        "filters": [{
          "name": "envoy.filters.network.tcp_proxy",
          "typed_config": {
            "@type": "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy",
            "stat_prefix": "sds",
            "cluster": "sds_backend"
          }
        }]
      }]
    }],
    "clusters": [{
      "name": "sds_backend",
      "type": "STATIC",
      "connect_timeout": "1s",
      "load_assignment": {
        "cluster_name": "sds_backend",
        "endpoints": [{"lb_endpoints": [{"endpoint": {"address": {
          "socket_address": {"address": "127.0.0.1", "port_value": 8080}
        }}}]}]
      }
    }]
  }
}`, jsonQuote(secretName), jsonQuote(path))
}

// TestSecretDeliveredOverSDSIsActive pins down where a delivered secret lands
// and what it is stamped with. A dynamic secret is the only resource kind whose
// version people routinely need -- "did the rotated cert actually land" -- so
// the section and version_info have to be the ones envoy-view reads.
func TestSecretDeliveredOverSDSIsActive(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "secret-sds-dynamic",
		Bootstrap: sdsListener("sds_cert", "/etc/envoy/sds.json"),
		Files: map[string]string{
			"sds.json": sdsResponse("1", tlsCertificateSecret("sds_cert")),
		},
	})

	ix := e.WaitFor("the SDS secret to arrive", func(ix *xds.Index) bool {
		return ix.Get(xds.KindSecret, "sds_cert") != nil
	})

	r := ix.Get(xds.KindSecret, "sds_cert")
	if r.State != xds.StateActive {
		t.Errorf("state = %q, want %q", r.State, xds.StateActive)
	}
	if r.VersionInfo != "1" {
		t.Errorf("version_info = %q, want %q", r.VersionInfo, "1")
	}
	if r.LastUpdated.IsZero() {
		t.Error("last_updated is zero; the UI shows this field for rotations")
	}
	if r.DecodeErr != nil {
		t.Errorf("secret did not decode: %v", r.DecodeErr)
	}

	// Delivery over SDS is not a way around redaction.
	if strings.Contains(string(r.Raw), pemBody(testKeyPEM)) {
		t.Errorf("a dynamically delivered secret leaks its private key into the dump:\n%s", r.Raw)
	}

	g := graph.Build(ix, graph.Options{})
	if edgeBetween(g, "listener/ingress_sds/fc/0", "secret/sds_cert") == nil {
		t.Errorf("no edge from the TLS filter chain to the SDS secret; nodes: %s", nodeIDs(g))
	}
	if n := nodeByID(g, "secret/sds_cert"); n == nil {
		t.Error("no secret node for the delivered secret")
	} else if !hasDetail(n, "version", "1") {
		t.Errorf("secret node details = %+v, want the delivered version shown", n.Details)
	}
}

// TestTLSUndeliveredSecretIsAWarmingPlaceholder is the failure this tool exists
// to make visible, and it does not look the way the graph builder assumes.
//
// The scenario is a control plane and a listener disagreeing about a name: the
// SDS file delivers "some_other_cert" while the transport socket asks for
// "missing_cert". The listener still comes up and holds its port, and every TLS
// handshake on it fails.
//
// The builder's unresolved-secret branch never fires here, because Envoy does
// not simply omit the secret. It publishes a placeholder into
// dynamic_warming_secrets: the requested name, version_info "uninitialized",
// and an empty body with no tls_certificate at all. So a name lookup succeeds
// and the graph draws an ordinary, healthy-looking secret node. The only clue
// on screen is the version reading "uninitialized".
//
// This test records that blind spot rather than endorsing it. The dump has
// everything needed to flag it -- warming state plus an empty secret body --
// and when the builder learns to, this test is what says so.
func TestTLSUndeliveredSecretIsAWarmingPlaceholder(t *testing.T) {
	e := envoytest.Start(t, envoytest.Config{
		Name:      "secret-dangling",
		Bootstrap: sdsListener("missing_cert", "/etc/envoy/sds.json"),
		Files: map[string]string{
			"sds.json": sdsResponse("1", tlsCertificateSecret("some_other_cert")),
		},
	})

	ix := e.Index()
	if l := ix.Get(xds.KindListener, "ingress_sds"); l == nil {
		t.Fatal("the listener is absent; Envoy dropped it rather than serving without a cert")
	}
	// The delivered-but-unwanted secret is dropped on the floor: SDS rejects a
	// response whose single resource is not the one it subscribed to, so the
	// name nobody asked for never reaches the dump either.
	if r := ix.Get(xds.KindSecret, "some_other_cert"); r != nil {
		t.Errorf("the mismatched secret was accepted after all: %+v", r)
	}

	r := ix.Get(xds.KindSecret, "missing_cert")
	if r == nil {
		t.Fatalf("no entry at all for the undelivered secret; the graph's unresolved "+
			"branch is reachable after all and this test should assert it\n%s", ix.Raw)
	}
	if r.State != xds.StateWarming {
		t.Errorf("state = %q, want %q", r.State, xds.StateWarming)
	}
	if r.VersionInfo != "uninitialized" {
		t.Errorf("version_info = %q, want %q", r.VersionInfo, "uninitialized")
	}
	if sec := secretMessage(t, r); sec.GetType() != nil {
		t.Errorf("placeholder secret carries a body (%T); it was supposed to be empty", sec.GetType())
	}

	g := graph.Build(ix, graph.Options{})
	if n := nodeByID(g, "unresolved/secret/missing_cert"); n != nil {
		t.Errorf("an unresolved node appeared: %+v", n)
	}

	n := nodeByID(g, "secret/missing_cert")
	if n == nil {
		t.Fatalf("no secret node for the placeholder; nodes: %s", nodeIDs(g))
	}
	if n.Status != graph.StatusOK {
		t.Errorf("node status = %q; the builder now flags an undelivered secret -- "+
			"good, assert the new status here and drop the blind-spot note", n.Status)
	}
	if !hasDetail(n, "version", "uninitialized") {
		t.Errorf("node details = %+v, want the %q version that is the only on-screen "+
			"hint that no cert arrived", n.Details, "uninitialized")
	}
	ed := edgeBetween(g, "listener/ingress_sds/fc/0", "secret/missing_cert")
	if ed == nil {
		t.Fatalf("no edge from the filter chain to the secret; edges: %s", edgeIDs(g))
	}
	if len(g.Problems) != 0 {
		t.Errorf("problems = %+v; a listener that cannot complete a handshake now "+
			"reaches the problems panel, so update this test", g.Problems)
	}

	// Delivering the name the listener actually wants flips the same entry to
	// active with a real version. That is what makes warming plus
	// "uninitialized" a signal worth acting on rather than a startup artifact.
	e.WriteFile("sds.json", sdsResponse("2", tlsCertificateSecret("missing_cert")))
	ix = e.WaitFor("the correctly named secret to arrive", func(ix *xds.Index) bool {
		r := ix.Get(xds.KindSecret, "missing_cert")
		return r != nil && r.State == xds.StateActive
	})

	r = ix.Get(xds.KindSecret, "missing_cert")
	if r.VersionInfo != "2" {
		t.Errorf("version_info after delivery = %q, want %q", r.VersionInfo, "2")
	}
	if secretMessage(t, r).GetTlsCertificate() == nil {
		t.Error("the active secret still carries no certificate")
	}
}

// ------------------------------------------------------------------- helpers

func nodeByID(g *graph.Graph, id graph.NodeID) *graph.Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

func hasDetail(n *graph.Node, label, value string) bool {
	for _, d := range n.Details {
		if d.Label == label && d.Value == value {
			return true
		}
	}
	return false
}

func edgeIDs(g *graph.Graph) string {
	var ids []string
	for _, e := range g.Edges {
		ids = append(ids, e.ID)
	}
	return strings.Join(ids, ", ")
}
