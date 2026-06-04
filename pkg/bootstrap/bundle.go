package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/operator"
)

// Bundle is the self-contained single-file deployment artifact produced by
// 'operator build'. It embeds the operator config, PKI material, and all
// fully-rendered per-service configs so that deployment needs no config dir.
type Bundle struct {
	// Version is the operator binary version embedded at build time.
	Version string `yaml:"version"`
	// BuiltAt is the Unix timestamp when the bundle was built.
	BuiltAt int64 `yaml:"built_at"`
	// Config is the operator.yaml content (settings only, no PKI).
	Config string `yaml:"config"`
	// Crypto is the crypto.yaml content (PKI material only).
	Crypto string `yaml:"crypto"`
	// CACert is the PEM-encoded root CA certificate.
	CACert string `yaml:"ca_cert"`
	// Services maps service name to its fully-rendered YAML config.
	// Kernel nodes are keyed as "bootstrap0", "bootstrap1", etc.
	Services map[string]string `yaml:"services"`
	// Seed is the one-time mint bootstrap-secrets file (seed.yaml content): the
	// PRIVATE half of each per-mint bootstrap key. It is delivered to mint,
	// consumed once, then DISCARDED — NEVER persisted to durable config/state.
	// Kept as a distinct top-level field (not under Crypto, which is durable PKI)
	// to reinforce its one-time lifecycle.
	Seed string `yaml:"seed,omitempty"`
}

// BuildBundle runs BuildDeployConfigs for cfgDir and returns a Bundle
// containing the fully-rendered output. This is the 'operator build' core.
func BuildBundle(cfgDir string) (*Bundle, error) {
	dc, err := BuildDeployConfigs(cfgDir)
	if err != nil {
		return nil, err
	}

	// Split config into settings (no PKI) and crypto (PKI only) for the bundle.
	settingsOnly := *dc.Cfg
	settingsOnly.Pki = nil
	settingsYAML, err := protoOperator.ConfigToYAML(&settingsOnly)
	if err != nil {
		return nil, fmt.Errorf("serializing operator config: %w", err)
	}

	cryptoOnly := &protoOperator.Config{Pki: dc.Cfg.Pki}
	cryptoYAML, err := protoOperator.ConfigToYAML(cryptoOnly)
	if err != nil {
		return nil, fmt.Errorf("serializing crypto: %w", err)
	}

	svcs := make(map[string]string)
	for i, bsYAML := range dc.BootstrapYAMLs {
		svcs[dc.BootstrapNames[i]] = string(bsYAML)
	}
	for _, pair := range []struct {
		name string
		data []byte
	}{
		{"sentinel", dc.SentinelConfig},
		{"gateway", dc.GatewayConfig},
		{"eventbus", dc.EventbusConfig},
		{"oauth", dc.OauthConfig},
		{"msggateway", dc.MsggatewayConfig},
		{"opa", dc.OpaConfig},
		{"anthropic", dc.AnthropicConfig},
		{"openai", dc.OpenAIConfig},
		{"saml", dc.SamlConfig},
		{"stratum", dc.StratumConfig},
		{"argus", dc.ArgusConfig},
		{"timedb", dc.TimedbConfig},
		{"graphdb", dc.GraphdbConfig},
		{"colddb", dc.ColddbConfig},
		{"letsencrypt", dc.LetsencryptConfig},
	} {
		if len(pair.data) > 0 {
			svcs[pair.name] = string(pair.data)
		}
	}

	return &Bundle{
		Version:  Version,
		BuiltAt:  time.Now().Unix(),
		Config:   string(settingsYAML),
		Crypto:   string(cryptoYAML),
		CACert:   string(dc.CACertPEM),
		Services: svcs,
		Seed:     string(dc.SeedYAML),
	}, nil
}

// LoadBundle reads and parses a bundle YAML file.
func LoadBundle(path string) (*Bundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading bundle %s: %w", path, err)
	}
	var b Bundle
	if err := yaml.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing bundle %s: %w", path, err)
	}
	return &b, nil
}

// SaveBundle writes a bundle to a YAML file.
func SaveBundle(b *Bundle, path string) error {
	data, err := yaml.Marshal(b)
	if err != nil {
		return fmt.Errorf("serializing bundle: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// BundleConfig reconstructs an OperatorConfig from a bundle by merging the
// settings and crypto YAML back together.
func BundleConfig(b *Bundle) (*protoOperator.Config, error) {
	cfg := &protoOperator.Config{}
	if b.Config != "" {
		part, err := protoOperator.ConfigFromBytes([]byte(b.Config))
		if err != nil {
			return nil, fmt.Errorf("parsing bundle config: %w", err)
		}
		proto.Merge(cfg, part)
	}
	if b.Crypto != "" {
		part, err := protoOperator.ConfigFromBytes([]byte(b.Crypto))
		if err != nil {
			return nil, fmt.Errorf("parsing bundle crypto: %w", err)
		}
		proto.Merge(cfg, part)
	}
	return cfg, nil
}

// BundleServiceValues converts a bundle's Services map to the Helm values
// format expected by the reconciler/apply flow: map of name → {"config": yaml}.
func BundleServiceValues(b *Bundle) map[string]interface{} {
	vals := make(map[string]interface{}, len(b.Services))
	for name, cfg := range b.Services {
		vals[name] = map[string]interface{}{"config": cfg}
	}
	return vals
}

// ExpandBundle writes the bundle contents into a directory layout:
//
//	<dir>/operator.yaml     — settings (durable)
//	<dir>/crypto.yaml       — PKI material (durable)
//	<dir>/seed.yaml         — one-time mint bootstrap secrets (consume once, discard)
//	<dir>/ca.pem            — root CA certificate
//	<dir>/configs/<svc>.yaml — rendered service configs
func ExpandBundle(b *Bundle, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0755); err != nil {
		return fmt.Errorf("creating output dirs: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "operator.yaml"), []byte(b.Config), 0644); err != nil {
		return fmt.Errorf("writing operator.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crypto.yaml"), []byte(b.Crypto), 0600); err != nil {
		return fmt.Errorf("writing crypto.yaml: %w", err)
	}
	// seed.yaml is the one-time bootstrap-secrets file (private mint bootstrap
	// keys). mode 0o600; consumed once by mint then discarded. Only written when
	// present so older bundles without a seed expand cleanly.
	if b.Seed != "" {
		if err := os.WriteFile(filepath.Join(dir, "seed.yaml"), []byte(b.Seed), 0600); err != nil {
			return fmt.Errorf("writing seed.yaml: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte(b.CACert), 0644); err != nil {
		return fmt.Errorf("writing ca.pem: %w", err)
	}

	for name, cfg := range b.Services {
		path := filepath.Join(dir, "configs", name+".yaml")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			return fmt.Errorf("writing configs/%s.yaml: %w", name, err)
		}
	}
	return nil
}

// PackBundle reads an expanded bundle directory and produces a Bundle.
// Expects operator.yaml, crypto.yaml, ca.pem, and configs/*.yaml.
func PackBundle(dir string) (*Bundle, error) {
	readFile := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", name, err)
		}
		return string(data), nil
	}

	config, err := readFile("operator.yaml")
	if err != nil {
		return nil, err
	}
	crypto, err := readFile("crypto.yaml")
	if err != nil {
		return nil, err
	}
	caCert, err := readFile("ca.pem")
	if err != nil {
		return nil, err
	}
	// seed.yaml is optional — only present when mint bootstrap keys were
	// provisioned. A missing seed.yaml is not an error.
	var seed string
	if data, err := os.ReadFile(filepath.Join(dir, "seed.yaml")); err == nil {
		seed = string(data)
	}

	configsDir := filepath.Join(dir, "configs")
	entries, err := os.ReadDir(configsDir)
	if err != nil {
		return nil, fmt.Errorf("reading configs dir: %w", err)
	}
	svcs := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		data, err := os.ReadFile(filepath.Join(configsDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading configs/%s: %w", e.Name(), err)
		}
		svcs[name] = string(data)
	}

	return &Bundle{
		Version:  Version,
		BuiltAt:  time.Now().Unix(),
		Config:   config,
		Crypto:   crypto,
		CACert:   caCert,
		Services: svcs,
		Seed:     seed,
	}, nil
}

// --- operator build ---

var buildOutputFile string

var buildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build a self-contained deployment bundle from the config directory",
	Long: `Build reads the operator config directory, renders all service configurations
(including OPA policies embedded in base64), and writes a single YAML bundle file.

The bundle is the canonical deployment artifact. Use it with:
  cavalier-operator apply --bundle <file>    deploy to Kubernetes
  cavalier-operator bundle expand <file>     unpack to a directory
  cavalier-operator generate --bundle <file> write Helm charts from the bundle

Examples:
  cavalier-operator build
  cavalier-operator build -o /tmp/release.yaml`,
	RunE: runBuild,
}

func init() {
	buildCmd.Flags().StringVarP(&buildOutputFile, "output", "o", "cavalier-bundle.yaml", "output bundle file path")
}

func runBuild(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()

	bundle, err := BuildBundle(dir)
	if err != nil {
		return err
	}

	if err := SaveBundle(bundle, buildOutputFile); err != nil {
		return err
	}

	fmt.Printf("Bundle written: %s\n", buildOutputFile)
	fmt.Printf("  version:  %s\n", bundle.Version)
	fmt.Printf("  services: %d\n", len(bundle.Services))
	fmt.Printf("\nDeploy with:\n")
	fmt.Printf("  cavalier-operator apply --bundle %s\n", buildOutputFile)
	return nil
}

// --- operator bundle ---

var bundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Convert between bundle file and directory layout",
}

var bundleExpandCmd = &cobra.Command{
	Use:   "expand <bundle-file> [output-dir]",
	Short: "Expand a bundle file into a directory",
	Long: `Expand unpacks a deployment bundle into a directory containing:
  operator.yaml   — cluster settings
  crypto.yaml     — PKI material (sensitive)
  ca.pem          — root CA certificate
  configs/        — rendered per-service YAML configs

Example:
  cavalier-operator bundle expand cavalier-bundle.yaml ./expanded`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runBundleExpand,
}

var bundlePackCmd = &cobra.Command{
	Use:   "pack <directory> [output-file]",
	Short: "Pack an expanded directory back into a bundle file",
	Long: `Pack reads an expanded bundle directory (from 'bundle expand') and
produces a single bundle YAML file.

Example:
  cavalier-operator bundle pack ./expanded cavalier-bundle.yaml`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runBundlePack,
}

func runBundleExpand(cmd *cobra.Command, args []string) error {
	bundlePath := args[0]
	outDir := "expanded"
	if len(args) == 2 {
		outDir = args[1]
	}

	b, err := LoadBundle(bundlePath)
	if err != nil {
		return err
	}

	if err := ExpandBundle(b, outDir); err != nil {
		return err
	}

	fmt.Printf("Bundle expanded: %s → %s/\n", bundlePath, outDir)
	fmt.Printf("  operator.yaml\n")
	fmt.Printf("  crypto.yaml\n")
	fmt.Printf("  ca.pem\n")
	fmt.Printf("  configs/ (%d services)\n", len(b.Services))
	return nil
}

func runBundlePack(cmd *cobra.Command, args []string) error {
	dir := args[0]
	outFile := "cavalier-bundle.yaml"
	if len(args) == 2 {
		outFile = args[1]
	}

	b, err := PackBundle(dir)
	if err != nil {
		return err
	}

	if err := SaveBundle(b, outFile); err != nil {
		return err
	}

	fmt.Printf("Bundle packed: %s → %s\n", dir, outFile)
	fmt.Printf("  services: %d\n", len(b.Services))
	return nil
}
