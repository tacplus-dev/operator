package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/operator"
)

const (
	OperatorSettingsFile = "operator.yaml"
	OperatorCryptoFile   = "crypto.yaml"
	OperatorBootstrapDir = "bootstrap"
)

// LoadConfig loads operator.yaml and crypto.yaml from cfgDir, deserializes
// both as OperatorConfig, and deep-merges them. Missing files are silently skipped.
func LoadConfig(cfgDir string) (*protoOperator.OperatorConfig, error) {
	cfg := &protoOperator.OperatorConfig{}

	settingsPath := filepath.Join(cfgDir, OperatorSettingsFile)
	if data, err := os.ReadFile(settingsPath); err == nil {
		part, err := protoOperator.OperatorConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", OperatorSettingsFile, err)
		}
		proto.Merge(cfg, part)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", OperatorSettingsFile, err)
	}

	cryptoPath := filepath.Join(cfgDir, OperatorCryptoFile)
	if data, err := os.ReadFile(cryptoPath); err == nil {
		part, err := protoOperator.OperatorConfigFromBytes(data)
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
func SaveSettings(cfgDir string, cfg *protoOperator.OperatorConfig) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	settings := proto.Clone(cfg).(*protoOperator.OperatorConfig)
	settings.Pki = nil

	data, err := protoOperator.OperatorConfigToYAML(settings)
	if err != nil {
		return fmt.Errorf("marshaling operator.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorSettingsFile), data, 0644)
}

// SaveCrypto writes only PKI material to crypto.yaml.
// Non-PKI fields are stripped before writing.
func SaveCrypto(cfgDir string, cfg *protoOperator.OperatorConfig) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	crypto := &protoOperator.OperatorConfig{Pki: cfg.Pki}

	data, err := protoOperator.OperatorConfigToYAML(crypto)
	if err != nil {
		return fmt.Errorf("marshaling crypto.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorCryptoFile), data, 0600)
}

// Issuer computes the full issuer URL from cfg.Fqdn and cfg.Port.
// Port 443 is omitted (standard HTTPS). Defaults to localhost:8443.
func Issuer(cfg *protoOperator.OperatorConfig) string {
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
func ValidateConfig(cfg *protoOperator.OperatorConfig) error {
	if cfg.Fqdn == "" {
		return fmt.Errorf("fqdn is required — run 'cavalier-operator init cluster --fqdn <fqdn>'")
	}
	if cfg.Pki == nil || cfg.Pki.RootCa == nil || len(cfg.Pki.RootCa.Chain) == 0 {
		return fmt.Errorf("pki.root_ca is required — run 'cavalier-operator init ca' first")
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
