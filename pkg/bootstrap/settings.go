package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

const (
	OperatorSettingsFile = "operator.yaml"
	OperatorCryptoFile   = "crypto.yaml"
	OperatorBootstrapDir = "bootstrap"
)

// LoadConfig loads operator.yaml and crypto.yaml from cfgDir, deserializes
// both as OperatorConfig, and deep-merges them. Missing files are silently skipped.
func LoadConfig(cfgDir string) (*protoOperator.Config, error) {
	cfg := &protoOperator.Config{}

	settingsPath := filepath.Join(cfgDir, OperatorSettingsFile)
	if data, err := os.ReadFile(settingsPath); err == nil {
		part, err := protoOperator.ConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", OperatorSettingsFile, err)
		}
		proto.Merge(cfg, part)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", OperatorSettingsFile, err)
	}

	cryptoPath := filepath.Join(cfgDir, OperatorCryptoFile)
	if data, err := os.ReadFile(cryptoPath); err == nil {
		part, err := protoOperator.ConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", OperatorCryptoFile, err)
		}
		proto.Merge(cfg, part)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", OperatorCryptoFile, err)
	}

	return cfg, nil
}

// SaveSettings writes non-PKI fields to operator.yaml.
// PKI material is stripped before writing (it lives in crypto.yaml).
func SaveSettings(cfgDir string, cfg *protoOperator.Config) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	settings := proto.Clone(cfg).(*protoOperator.Config)
	settings.Pki = nil

	data, err := protoOperator.ConfigToYAML(settings)
	if err != nil {
		return fmt.Errorf("marshaling operator.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorSettingsFile), data, 0644)
}

// SaveCrypto writes only PKI material to crypto.yaml.
// Non-PKI fields are stripped before writing.
func SaveCrypto(cfgDir string, cfg *protoOperator.Config) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	crypto := &protoOperator.Config{Pki: cfg.Pki}

	data, err := protoOperator.ConfigToYAML(crypto)
	if err != nil {
		return fmt.Errorf("marshaling crypto.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorCryptoFile), data, 0600)
}

// Issuer computes the full issuer URL from cfg.Fqdn and cfg.Port.
// Port 443 is omitted (standard HTTPS). Defaults to localhost:8443.
func Issuer(cfg *protoOperator.Config) string {
	fqdn := cfg.Fqdn
	if fqdn == "" {
		fqdn = "localhost"
	}
	port := cfg.Port
	if port == 0 {
		port = 8443
	}
	if port == 443 {
		return "https://" + fqdn
	}
	return fmt.Sprintf("https://%s:%d", fqdn, port)
}

// ValidateConfig checks that cfg is internally consistent for deployment.
func ValidateConfig(cfg *protoOperator.Config) error {
	if cfg.Fqdn == "" {
		return fmt.Errorf("fqdn is required — run 'schematic-operator init cluster --fqdn <fqdn>'")
	}
	if cfg.Pki == nil || cfg.Pki.RootCa == nil || len(cfg.Pki.RootCa.Chain) == 0 {
		return fmt.Errorf("pki.root_ca is required — run 'schematic-operator init ca' first")
	}
	if cfg.Tls != nil && !cfg.Tls.Letsencrypt {
		if cfg.Tls.Ca == nil || len(cfg.Tls.Ca.Chain) == 0 {
			return fmt.Errorf("tls.ca is required when letsencrypt = false")
		}
	}
	if cfg.GetMode() == protoOperator.OperatorMode_GENERATE_ONLY {
		if cfg.Topology != nil && cfg.Topology.GatewayNodes == 0 {
			return fmt.Errorf("topology.gateway_nodes must be set explicitly in GENERATE_ONLY mode")
		}
	}
	return nil
}
