package bootstrap

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	protoGateway "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/gateway"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoHost "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/host"
	protoX509 "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/x509"
)

var (
	kernelGRPCPort = int32(protoKernel.DefaultListenerPort)
	gatewayPort    = int32(protoGateway.DefaultTcpListenerPort)
)

const (
	// gatewayHttpPort is the container-side port the gateway binds for the
	// edge's PLAINTEXT port 80 — ACME HTTP-01 challenges proxied to
	// letsencrypt, and a 301 to https for everything else. The gateway's
	// LoadBalancer maps external 80 to it, so the container binds a port above
	// 1024 and needs no NET_BIND_SERVICE. Must match helm's gatewayHttpPort.
	gatewayHttpPort = int32(8080)

	// letsencryptAcmePort is the letsencrypt service's HTTP-01 responder port,
	// reachable in-cluster only: nothing outside the cluster reaches it any
	// more, only the gateway's port-80 proxy.
	letsencryptAcmePort = int32(8080)
)

// letsencryptAcmeHost is the in-cluster DNS name of that responder. The
// letsencrypt service has exactly one replica, so its per-replica ClusterIP
// Service is "letsencrypt0". Written in full svc.cluster.local form because
// host.Host asserts an FQDN and a bare single label is not one — and the
// short "letsencrypt0.<ns>" form only passes that assertion for namespaces
// whose name happens to end in letters.
func letsencryptAcmeHost(cfg *protoOperator.Config) string {
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}
	return fmt.Sprintf("letsencrypt0.%s.svc.cluster.local", ns)
}

func defaultLoggerFields() map[string]any {
	return map[string]any{
		"format":       "format-json",
		"level":        "level-debug",
		"output_paths": []string{"stdout"},
	}
}

func applyServiceDefaults(fields map[string]any) {
	fields["listen_address"] = map[string]any{"host": "0.0.0.0", "port": int64(8443)}
	fields["max_request_size"] = int64(10 * 1024 * 1024)
	fields["request_timeout_seconds"] = int64(30)
	fields["shutdown_timeout_seconds"] = int64(30)
	fields["logger"] = defaultLoggerFields()
}

func normalizeBase64URLNoPad(encoded string) (string, error) {
	decoders := []*base64.Encoding{
		base64.RawURLEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.StdEncoding,
	}
	for _, decoder := range decoders {
		if decoded, err := decoder.DecodeString(encoded); err == nil {
			return base64.RawURLEncoding.EncodeToString(decoded), nil
		}
	}
	return "", fmt.Errorf("invalid base64 payload")
}

func normalizeBase64Field(m map[string]any) error {
	raw, ok := m["base64"]
	if !ok {
		return nil
	}
	encoded, ok := raw.(string)
	if !ok || encoded == "" {
		return nil
	}
	normalized, err := normalizeBase64URLNoPad(encoded)
	if err != nil {
		return err
	}
	m["base64"] = normalized
	return nil
}

// BuildBackendFields returns the backend connection YAML map fields
// (endpoints, trusted_cas, spiffe_trust_domain, location_path) for services
// connecting to kernel. Service bootstrap now uses projected Kubernetes tokens,
// so no service cred_pair private key is emitted into bootstrap config.
func BuildBackendFields(trustedCAs []*protoX509.X509Cert, endpoints []*protoHost.Host, spiffeTrustDomain, locationPath string) (map[string]any, error) {
	var trustedCAsSlice []any
	for _, ca := range trustedCAs {
		caYAML, err := protoX509.X509CertToYAML(ca)
		if err != nil {
			return nil, fmt.Errorf("marshaling trusted CA: %w", err)
		}
		var caMap map[string]any
		if err := yaml.Unmarshal(caYAML, &caMap); err != nil {
			return nil, fmt.Errorf("unmarshaling trusted CA YAML: %w", err)
		}
		if err := normalizeBase64Field(caMap); err != nil {
			return nil, fmt.Errorf("normalizing trusted CA base64: %w", err)
		}
		trustedCAsSlice = append(trustedCAsSlice, caMap)
	}

	var endpointsSlice []any
	for _, ep := range endpoints {
		endpointsSlice = append(endpointsSlice, map[string]any{
			"host": ep.Host,
			"port": int64(ep.Port),
		})
	}

	out := map[string]any{
		"endpoints":   endpointsSlice,
		"trusted_cas": trustedCAsSlice,
	}
	if spiffeTrustDomain != "" {
		out["spiffe_trust_domain"] = spiffeTrustDomain
	}
	if locationPath != "" {
		out["location_path"] = locationPath
	}
	return out, nil
}

// GatewayOptions holds user-configurable gateway settings from services/gateway.yaml.
type GatewayOptions struct {
	EnableTCP       bool
	EnableQUIC      bool
	LetsEncrypt     bool
	Hostnames       []string
	ExternalCertPEM string
	ExternalKeyPEM  string
	RootCAs         []*protoX509.X509Cert
}

// BuildGatewayServiceConfig generates the serviceconfig YAML for gateway.
func BuildGatewayServiceConfig(
	trustedCAs []*protoX509.X509Cert,
	opts GatewayOptions,
	spiffeTrustDomain string,
	locationPath string,
	endpoints ...*protoHost.Host,
) ([]byte, error) {
	config, err := BuildBackendFields(trustedCAs, endpoints, spiffeTrustDomain, locationPath)
	if err != nil {
		return nil, err
	}

	config["tcp_listener"] = map[string]any{"host": "0.0.0.0", "port": int64(gatewayPort)}
	config["quic_listener"] = map[string]any{"host": "0.0.0.0", "port": int64(gatewayPort)}
	config["max_request_size"] = int64(10 * 1024 * 1024)
	config["request_timeout_seconds"] = int64(30)
	config["shutdown_timeout_seconds"] = int64(30)
	config["logger"] = defaultLoggerFields()
	config["enable_tcp"] = opts.EnableTCP
	config["enable_quic"] = opts.EnableQUIC
	if opts.LetsEncrypt {
		config["external_tls_mode"] = "EXTERNAL_TLS_MODE_LETSENCRYPT"
	} else {
		config["external_tls_mode"] = "EXTERNAL_TLS_MODE_CERT_CHAIN"
	}
	if len(opts.Hostnames) > 0 {
		config["external_hostnames"] = opts.Hostnames
	}
	if opts.ExternalCertPEM != "" {
		config["external_cert_pem"] = opts.ExternalCertPEM
	}
	if opts.ExternalKeyPEM != "" {
		config["external_key_pem"] = opts.ExternalKeyPEM
	}
	if len(opts.RootCAs) > 0 {
		var rootCASlice []any
		for _, ca := range opts.RootCAs {
			caYAML, err := protoX509.X509CertToYAML(ca)
			if err != nil {
				return nil, fmt.Errorf("marshaling gateway root CA: %w", err)
			}
			var caMap map[string]any
			if err := yaml.Unmarshal(caYAML, &caMap); err != nil {
				return nil, fmt.Errorf("unmarshaling gateway root CA YAML: %w", err)
			}
			if err := normalizeBase64Field(caMap); err != nil {
				return nil, fmt.Errorf("normalizing gateway root CA base64: %w", err)
			}
			rootCASlice = append(rootCASlice, caMap)
		}
		config["root_ca"] = rootCASlice
	}

	return yaml.Marshal(config)
}

// LoadGatewayOptions reads the gateway service config and returns options.
// Returns defaults (both enabled) if the file doesn't exist or is empty.
func LoadGatewayOptions(servicesDir string) GatewayOptions {
	opts := GatewayOptions{EnableTCP: true, EnableQUIC: true}

	data, err := os.ReadFile(filepath.Join(servicesDir, "gateway.yaml"))
	if err != nil {
		return opts
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		return opts
	}

	if v, ok := raw["enable_tcp"]; ok {
		if b, ok := v.(bool); ok {
			opts.EnableTCP = b
		}
	}
	if v, ok := raw["enable_quic"]; ok {
		if b, ok := v.(bool); ok {
			opts.EnableQUIC = b
		}
	}
	if v, ok := raw["letsencrypt"]; ok {
		if b, ok := v.(bool); ok && b {
			opts.LetsEncrypt = true
		}
	}
	return opts
}

// LoadDeployList reads a deploy.list file from the config directory.
// Returns nil if the file does not exist (no filtering).
func LoadDeployList(cfgDir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(cfgDir, "deploy.list"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading deploy.list: %w", err)
	}

	allowed := make(map[string]bool)
	for _, line := range splitLines(string(data)) {
		if line == "" || line[0] == '#' {
			continue
		}
		parts := splitPath(line)
		allowed[parts[len(parts)-1]] = true
	}
	return allowed, nil
}
