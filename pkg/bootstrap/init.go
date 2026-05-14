package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/helm"
	"gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/reconciler"
	"gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/upgrade"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/utils/operator"
)

// --- init keycloak ---

var initKeycloakCmd = &cobra.Command{
	Use:   "keycloak",
	Short: "Enable Keycloak IDP — generate secrets and configure OAuth",
	Long: `Enable Keycloak as the identity provider for a dev or POC cluster.

Generates secrets (admin password, OAuth client secret, default user passwords),
enables keycloak and oauth in optional_services, and adds the Keycloak provider
to services/oauth.yaml.

Idempotent: skips steps that are already done.

Example:
  cavalier-operator init keycloak`,
	RunE: runInitKeycloak,
}

func runInitKeycloak(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	if cfg.Fqdn == "" {
		return fmt.Errorf("fqdn not set — run 'cavalier-operator init cluster --fqdn <fqdn>' first")
	}

	// Generate keycloak secrets (idempotent).
	if err := generateKeycloakSecrets(dir); err != nil {
		return fmt.Errorf("generating keycloak secrets: %w", err)
	}

	secrets, err := LoadKeycloakSecrets(dir)
	if err != nil {
		return fmt.Errorf("loading keycloak secrets: %w", err)
	}

	// Enable keycloak and oauth in optional_services.
	changed := false
	if cfg.OptionalServices == nil {
		cfg.OptionalServices = &protoOperator.OptionalServices{}
	}
	if !cfg.OptionalServices.Keycloak {
		cfg.OptionalServices.Keycloak = true
		changed = true
	}
	if !cfg.OptionalServices.Oauth {
		cfg.OptionalServices.Oauth = true
		changed = true
	}
	if changed {
		if err := SaveSettings(dir, cfg); err != nil {
			return err
		}
	}

	// Add keycloak provider to services/oauth.yaml.
	realm := "cavalier"
	keycloakIssuer := fmt.Sprintf("https://%s/realms/%s", cfg.Fqdn, realm)
	if err := addKeycloakOAuthProvider(dir, keycloakIssuer, secrets.OauthClientSecret); err != nil {
		return err
	}

	fmt.Printf("Keycloak enabled for cluster %s\n", cfg.Fqdn)
	fmt.Printf("  services/keycloak-generated.yaml   secrets\n")
	fmt.Printf("  services/oauth.yaml                keycloak provider added\n")
	fmt.Printf("  operator.yaml                      optional_services.keycloak = true\n")
	fmt.Printf("\nDefault users:\n")
	fmt.Printf("  admin    password: %s\n", secrets.UserPasswords["admin"])
	fmt.Printf("  joeuser  password: %s\n", secrets.UserPasswords["joeuser"])
	fmt.Printf("\nRun 'cavalier-operator build' to generate the deployment bundle.\n")
	return nil
}

// addKeycloakOAuthProvider adds a Keycloak provider entry to services/oauth.yaml.
// Idempotent: skips if the issuer is already present.
func addKeycloakOAuthProvider(cfgDir, issuer, clientSecret string) error {
	oauthPath := filepath.Join(cfgDir, "services", "oauth.yaml")

	var cfg map[string]any
	if data, err := os.ReadFile(oauthPath); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing oauth.yaml: %w", err)
		}
	} else if os.IsNotExist(err) {
		cfg = map[string]any{}
	} else {
		return fmt.Errorf("reading oauth.yaml: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}

	// Check if provider already present.
	if providers, ok := cfg["providers"].([]any); ok {
		for _, p := range providers {
			if pm, ok := p.(map[string]any); ok {
				if pm["issuer"] == issuer {
					fmt.Printf("  services/oauth.yaml: keycloak provider already present\n")
					return nil
				}
			}
		}
	}

	provider := map[string]any{
		"enable":        true,
		"name":          "Keycloak",
		"issuer":        issuer,
		"client_id":     "cavalier",
		"client_secret": clientSecret,
		"scopes":        []any{"openid", "profile", "email"},
	}

	providers, _ := cfg["providers"].([]any)
	cfg["providers"] = append(providers, provider)
	cfg["enable"] = true

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling oauth.yaml: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfgDir, "services"), 0755); err != nil {
		return fmt.Errorf("creating services dir: %w", err)
	}
	return os.WriteFile(oauthPath, data, 0600)
}

// --- run (default) ---

func runOperator(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}

	if err := ValidateConfig(cfg); err != nil {
		return err
	}

	log, _ := zap.NewProduction()
	defer log.Sync()

	switch cfg.GetMode() {
	case protoOperator.OperatorMode_GENERATE_ONLY:
		return runGenerate(cfg, log)

	default: // IN_CLUSTER
		return runInCluster(dir, cfg, log)
	}
}

func runInCluster(dir string, cfg *protoOperator.OperatorConfig, log *zap.Logger) error {
	log.Info("starting in-cluster operator", zap.String("config_dir", dir))

	restCfg, err := ctrlcfg.GetConfig()
	if err != nil {
		return fmt.Errorf("getting kubeconfig: %w", err)
	}

	mgr, err := manager.New(restCfg, manager.Options{
		LeaderElection:          true,
		LeaderElectionID:        "cavalier-operator-leader",
		LeaderElectionNamespace: cfg.GetNamespace(),
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// Determine the operator's own namespace for reading secrets (e.g. gitlab-registry).
	// Standard Kubernetes pattern: inject via POD_NAMESPACE env var in the Deployment spec.
	operatorNS := os.Getenv("POD_NAMESPACE")
	if operatorNS == "" {
		operatorNS = "cavalier-operator"
	}

	provider := NewServiceConfigProvider(dir)
	checker := upgrade.New(
		CurrentMajorMinor(),
		CurrentSeries(),
		CurrentProtoPatch(),
		CurrentBranch(),
		0,       // localPatch=0 → auto-resolve latest
		Version, // fallbackTag: operator's own version, used when resolution fails
		upgrade.AllServices(),
		mgr.GetClient(),
		operatorNS,
		log,
	)
	rec := reconciler.New(cfg, provider, checker, mgr, log, operatorNS, CurrentSeries(), CurrentMajorMinor())

	// Register as a managed runnable so the manager starts it AFTER the k8s
	// cache is ready. This prevents the "cache is not started" error when
	// Resolve() tries to read the gitlab-registry secret on startup.
	if err := mgr.Add(rec); err != nil {
		return fmt.Errorf("registering reconciler: %w", err)
	}

	return mgr.Start(context.Background())
}

func runGenerate(cfg *protoOperator.OperatorConfig, log *zap.Logger) error {
	topo := cfg.GetTopology()
	if topo == nil || topo.GatewayNodes == 0 {
		return fmt.Errorf("topology.gateway_nodes must be set in GENERATE_ONLY mode")
	}

	g := helm.New(CurrentSeries(), CurrentMajorMinor(), nil)
	charts, err := g.Generate(cfg, 0)
	if err != nil {
		return err
	}

	outDir := cfg.OutputDir
	if outDir == "" {
		outDir = "charts"
	}

	if err := g.WriteToDisk(charts, outDir); err != nil {
		return err
	}

	log.Info("charts written", zap.String("dir", outDir), zap.Int("files", len(charts.Files)))
	fmt.Printf("Generated %d chart files → %s/cavalier/\n", len(charts.Files), outDir)
	return nil
}

// --- init ---

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize operator configuration",
}

// --- init ca ---

var initCACmd = &cobra.Command{
	Use:   "ca",
	Short: "Generate root CA → crypto.yaml",
	Long: `Generate a root CA and write it to crypto.yaml in the config directory.

Idempotent: skips if a root CA already exists in crypto.yaml.

Example:
  cavalier-operator init ca
  cavalier-operator init ca -c /path/to/config`,
	RunE: runInitCA,
}

func runInitCA(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}

	if cfg.Pki != nil && cfg.Pki.RootCa != nil && len(cfg.Pki.RootCa.Chain) > 0 {
		fmt.Printf("Root CA already exists in %s/crypto.yaml\n", dir)
		return nil
	}

	if _, _, err := GenerateRootCA(cfg); err != nil {
		return fmt.Errorf("generating root CA: %w", err)
	}

	if err := SaveCrypto(dir, cfg); err != nil {
		return err
	}

	caPEM, err := RootCACertPEM(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), caPEM, 0644); err != nil {
		return fmt.Errorf("writing ca.pem: %w", err)
	}

	fmt.Printf("Root CA created: %s/\n", dir)
	fmt.Printf("  crypto.yaml   root CA certificate + private key\n")
	fmt.Printf("  ca.pem        PEM-encoded root CA certificate (add to host trust)\n")
	return nil
}

// --- init cluster ---

var (
	initClusterFQDN string
	initClusterPort int
	initClusterCA   string
	initClusterType string
	initClusterGW   int
)

var initClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Initialize cluster PKI and operator.yaml",
	Long: `Generate cluster PKI material and write operator.yaml.

Requires a root CA (from 'cavalier-operator init ca' or an existing crypto.yaml).

Flags:
  --ca       Path to crypto.yaml containing the root CA (required if not in config dir).
  --fqdn     Public cluster FQDN (e.g. api.example.com). Required.
  --port     Gateway HTTPS port. Default 443.
  --type     Topology preset: small (default), medium, large.
  --gateways Number of gateway replicas (0 = auto from node count in IN_CLUSTER mode).

Example:
  cavalier-operator init cluster --ca ../ca/crypto.yaml --fqdn api.example.com
  cavalier-operator init cluster --ca ../ca/crypto.yaml --fqdn api.example.com --type medium --gateways 3`,
	RunE: runInitCluster,
}

func init() {
	initClusterCmd.Flags().StringVar(&initClusterFQDN, "fqdn", "", "public cluster FQDN (required)")
	initClusterCmd.Flags().IntVar(&initClusterPort, "port", 443, "gateway HTTPS port")
	initClusterCmd.Flags().StringVar(&initClusterCA, "ca", "", "path to root CA crypto.yaml")
	initClusterCmd.Flags().StringVar(&initClusterType, "type", "small", "topology preset: small, medium, large")
	initClusterCmd.Flags().IntVar(&initClusterGW, "gateways", 0, "gateway replicas (0 = auto from node count)")
}

func runInitCluster(cmd *cobra.Command, args []string) error {
	if initClusterFQDN == "" {
		return fmt.Errorf("--fqdn is required")
	}

	dir := resolveConfigDir()

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}

	// Load root CA from external file if provided.
	if initClusterCA != "" {
		caDir := initClusterCA
		// If the path is a file, use its directory.
		if fi, err := os.Stat(initClusterCA); err == nil && !fi.IsDir() {
			caDir = filepath.Dir(initClusterCA)
		}
		caCfg, err := LoadConfig(caDir)
		if err != nil {
			return fmt.Errorf("reading CA config: %w", err)
		}
		if caCfg.Pki == nil || caCfg.Pki.RootCa == nil {
			return fmt.Errorf("no root_ca found in %s", initClusterCA)
		}
		if cfg.Pki == nil {
			cfg.Pki = &protoOperator.PKI{}
		}
		cfg.Pki.RootCa = caCfg.Pki.RootCa
	}

	if cfg.Pki == nil || cfg.Pki.RootCa == nil {
		return fmt.Errorf("no root CA found — run 'cavalier-operator init ca' first or provide --ca")
	}

	cfg.Fqdn = initClusterFQDN
	cfg.Port = int32(initClusterPort)
	ApplyTopologyPreset(cfg, initClusterType)
	if initClusterGW > 0 {
		cfg.Topology.GatewayNodes = int32(initClusterGW)
	}
	topo := cfg.Topology

	// Generate cluster PKI.
	nodeCount := int(topo.KernelNodes)
	if nodeCount == 0 {
		nodeCount = 1
	}
	if err := GenerateCrypto(cfg, nodeCount, 0, initClusterFQDN); err != nil {
		return fmt.Errorf("generating cluster PKI: %w", err)
	}

	// Write services/ and creds/ if not already present.
	if _, err := os.Stat(filepath.Join(dir, "services")); os.IsNotExist(err) {
		fmt.Println("Creating services/...")
		if err := WriteServices(dir); err != nil {
			return err
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "creds")); os.IsNotExist(err) {
		fmt.Println("Creating creds/...")
		if err := WriteCreds(dir); err != nil {
			return err
		}
	}

	if err := SaveSettings(dir, cfg); err != nil {
		return err
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		return err
	}

	// Write ca.pem for host trust.
	caPEM, err := RootCACertPEM(cfg)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), caPEM, 0644); err != nil {
		return fmt.Errorf("writing ca.pem: %w", err)
	}

	opt := cfg.GetOptionalServices()

	oauthStatus := "disabled"
	if opt.GetOauth() {
		oauthStatus = "enabled"
	}
	samlStatus := "disabled"
	if opt.GetSaml() {
		samlStatus = "enabled"
	}

	fmt.Printf("\nCluster initialized: %s/\n", dir)
	fmt.Printf("  fqdn:       %s\n", cfg.Fqdn)
	fmt.Printf("  issuer:     %s\n", Issuer(cfg))
	fmt.Printf("  port:       %d\n", cfg.Port)
	fmt.Printf("  kernel:     %d\n", topo.KernelNodes)
	fmt.Printf("  sentinel:   %d\n", topo.SentinelNodes)
	fmt.Printf("  gateway:    %d (0 = auto from node count)\n", topo.GatewayNodes)
	fmt.Printf("  eventbus:   %d\n", topo.EventbusNodes)
	fmt.Printf("  oauth:  %s\n", oauthStatus)
	fmt.Printf("  opa:   %d\n", topo.OpaNodes)
	fmt.Printf("  argus:      %d\n", topo.ArgusNodes)
	fmt.Printf("  timedb:     %d\n", topo.TimedbNodes)
	fmt.Printf("  graphdb:    %d\n", topo.GraphdbNodes)
	fmt.Printf("  stratum:    %d\n", topo.StratumNodes)
	fmt.Printf("  saml:       %s\n", samlStatus)
	fmt.Printf("  services/   editable service configs\n")
	fmt.Printf("  creds/      credential pairs\n")
	fmt.Printf("  ca.pem      CA cert for host trust\n")
	fmt.Printf("\nEdit services/*.yaml, then run:\n")
	fmt.Printf("  cavalier-operator generate\n")
	return nil
}
