package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	protoHost "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/libs/host"
	protoKey "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/libs/key"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/libs/x509"
	protoGateway "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/services/gateway"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/services/kernel"
	protoSentinel "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/services/sentinel"
)

var (
	kernelGRPCPort = int32(protoKernel.DefaultListenerPort)
	sentinelPort   = int32(protoSentinel.DefaultPort)
	gatewayPort    = int32(protoGateway.DefaultTcpListenerPort)
)

// BuildBackendFields returns the backend connection YAML map fields
// (endpoints, private_key, trusted_cas) for services connecting to kernel.
func BuildBackendFields(privateKey *protoKey.Key, trustedCAs []*protoX509.X509Cert, endpoints []*protoHost.Host) (map[string]any, error) {
	privateKeyYAML, err := protoKey.KeyToYAML(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshaling private_key: %w", err)
	}
	var privateKeyMap map[string]any
	if err := yaml.Unmarshal(privateKeyYAML, &privateKeyMap); err != nil {
		return nil, fmt.Errorf("unmarshaling private_key YAML: %w", err)
	}

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
		trustedCAsSlice = append(trustedCAsSlice, caMap)
	}

	var endpointsSlice []any
	for _, ep := range endpoints {
		endpointsSlice = append(endpointsSlice, map[string]any{
			"host": ep.Host,
			"port": int64(ep.Port),
		})
	}

	return map[string]any{
		"endpoints":   endpointsSlice,
		"private_key": privateKeyMap,
		"trusted_cas": trustedCAsSlice,
	}, nil
}

// GatewayOptions holds user-configurable gateway settings from services/gateway.yaml.
type GatewayOptions struct {
	EnableTCP    bool
	EnableQUIC   bool
	LetsEncrypt  bool
	Hostnames    []string
}

// BuildGatewayServiceConfig generates the serviceconfig YAML for gateway.
func BuildGatewayServiceConfig(
	privateKey *protoKey.Key,
	trustedCAs []*protoX509.X509Cert,
	opts GatewayOptions,
	endpoints ...*protoHost.Host,
) ([]byte, error) {
	config, err := BuildBackendFields(privateKey, trustedCAs, endpoints)
	if err != nil {
		return nil, err
	}

	config["tcp_listener"] = map[string]any{"host": "0.0.0.0", "port": int64(gatewayPort)}
	config["quic_listener"] = map[string]any{"host": "0.0.0.0", "port": int64(gatewayPort)}
	config["max_request_size"] = int64(10 * 1024 * 1024)
	config["request_timeout_seconds"] = int64(30)
	config["shutdown_timeout_seconds"] = int64(30)
	config["logger"] = map[string]any{
		"level":        "LEVEL_DEBUG",
		"output_paths": []string{"stdout"},
	}
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
