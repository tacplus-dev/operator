package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

const (
	OperatorSettingsFile = "operator.yaml"
	OperatorCryptoFile   = "crypto.yaml"
	// OperatorImagesFile is the release manifest scripts/promote.sh writes: an
	// operator Config carrying only image_digests.
	OperatorImagesFile   = "images.yaml"
	OperatorBootstrapDir = "bootstrap"
)

// LoadConfig loads operator.yaml, crypto.yaml and images.yaml from cfgDir,
// deserializes each as OperatorConfig, and deep-merges them. Missing files are
// silently skipped.
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

	imagesPath := filepath.Join(cfgDir, OperatorImagesFile)
	if data, err := os.ReadFile(imagesPath); err == nil {
		part, err := protoOperator.ConfigFromBytes(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", OperatorImagesFile, err)
		}
		proto.Merge(cfg, part)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", OperatorImagesFile, err)
	}

	// proto.Merge appends repeated fields rather than replacing them, so a
	// field carried by BOTH files arrives duplicated. llm_credential_services
	// is the only repeated field either writer can emit into either file
	// (SaveSettings nils it, SaveCrypto populates it), which makes it the one
	// that duplicates whenever an operator.yaml predates that split. It is a
	// set of app service names, so order does not carry meaning beyond
	// determinism — keep first occurrence, drop the rest.
	cfg.LlmCredentialServices = dedupe(cfg.LlmCredentialServices)

	return cfg, nil
}

// dedupe returns in with duplicate entries removed, preserving first-seen order.
func dedupe(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// SaveSettings writes operator.yaml. PKI material and the shared LLM
// credential are stripped before writing (they live in crypto.yaml), and so
// are image digests (they live in images.yaml).
func SaveSettings(cfgDir string, cfg *protoOperator.Config) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	settings := proto.Clone(cfg).(*protoOperator.Config)
	settings.Pki = nil
	settings.LlmCredential = ""
	settings.LlmCredentialServices = nil
	settings.ImageDigests = nil

	data, err := protoOperator.ConfigToYAML(settings)
	if err != nil {
		return fmt.Errorf("marshaling operator.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorSettingsFile), data, 0644)
}

// SaveCrypto writes PKI material and the shared LLM credential to crypto.yaml.
// All other fields are stripped before writing (they live in operator.yaml).
func SaveCrypto(cfgDir string, cfg *protoOperator.Config) error {
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	crypto := &protoOperator.Config{
		Pki:                   cfg.Pki,
		LlmCredential:         cfg.LlmCredential,
		LlmCredentialServices: cfg.LlmCredentialServices,
	}

	data, err := protoOperator.ConfigToYAML(crypto)
	if err != nil {
		return fmt.Errorf("marshaling crypto.yaml: %w", err)
	}

	return os.WriteFile(filepath.Join(cfgDir, OperatorCryptoFile), data, 0600)
}

// retiredPKIKeys are crypto.yaml keys under pki whose fields are reserved now
// but which held private keys in files already written. Each is listed in
// both spellings ConfigFromBytes accepts.
var retiredPKIKeys = []string{"kmaker_signing_ca", "kmakerSigningCa"}

// cryptoHoldsRetiredKeys reports whether cfgDir's crypto.yaml still carries a
// retired private key. LoadConfig drops unknown fields, so the key never
// reaches cfg and nothing would rewrite the file on its own; a caller that
// sees true saves crypto.yaml again. An unreadable file reports false:
// LoadConfig has already reported it.
func cryptoHoldsRetiredKeys(cfgDir string) bool {
	data, err := os.ReadFile(filepath.Join(cfgDir, OperatorCryptoFile))
	if err != nil {
		return false
	}
	var crypto struct {
		Pki map[string]any `yaml:"pki"`
	}
	if err := yaml.Unmarshal(data, &crypto); err != nil {
		return false
	}
	for _, k := range retiredPKIKeys {
		if _, ok := crypto.Pki[k]; ok {
			return true
		}
	}
	return false
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
