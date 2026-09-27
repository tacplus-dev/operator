package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	loggerchassis "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/logger"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/reconciler"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/upgrade"
)

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

	// Through the chassis, not straight from zap: it wraps the sink in a redactor
	// so a credential cannot reach the log through a structured FIELD. This path
	// runs `operator run`, which reconciles a cluster holding the bootstrap crypto,
	// and the same bypass was found in kmaker and letsencrypt by the same sweep.
	log, err := loggerchassis.InstallFromConfig(nil)
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer log.Sync() //nolint:errcheck

	switch cfg.GetMode() {
	case protoOperator.OperatorMode_GENERATE_ONLY:
		return runGenerate(cfg, log)

	default: // IN_CLUSTER
		return runInCluster(dir, cfg, log)
	}
}

func runInCluster(dir string, cfg *protoOperator.Config, log *zap.Logger) error {
	log.Info("starting in-cluster operator", zap.String("config_dir", dir))

	restCfg, err := ctrlcfg.GetConfig()
	if err != nil {
		return fmt.Errorf("getting kubeconfig: %w", err)
	}

	// Determine the operator's own namespace for reading secrets (e.g. gitlab-registry).
	// Standard Kubernetes pattern: inject via POD_NAMESPACE env var in the Deployment spec.
	operatorNS := os.Getenv("POD_NAMESPACE")
	if operatorNS == "" {
		operatorNS = "schematic-operator"
	}

	targetNS := cfg.GetNamespace()
	if targetNS == "" {
		targetNS = reconciler.DefaultNamespace
	}

	// Confine the cache to the namespaces the operator actually manages. Without
	// this the informers LIST/WATCH secrets, deployments, statefulsets, services
	// and configmaps cluster-wide, which the namespaced Role in the chart does not
	// grant. Cluster-scoped types (nodes) still use the cluster-wide informer.
	mgr, err := manager.New(restCfg, manager.Options{
		LeaderElection:          true,
		LeaderElectionID:        "schematic-operator-leader",
		LeaderElectionNamespace: targetNS,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				operatorNS: {},
				targetNS:   {},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	provider := NewServiceConfigProvider(dir)
	pollInterval := upgrade.PollInterval
	if cfg.Dev {
		pollInterval = upgrade.DevPollInterval
	}
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
		pollInterval,
	)
	tenantOwned, err := TenantOwnedServices()
	if err != nil {
		return fmt.Errorf("reading tenant-owned services from registry: %w", err)
	}
	pinnedTenant, err := PinnedTenantServices()
	if err != nil {
		return fmt.Errorf("reading pinned-tenant services from registry: %w", err)
	}
	rec := reconciler.New(cfg, provider, checker, mgr, log, operatorNS, CurrentSeries(), CurrentMajorMinor(), tenantOwned, pinnedTenant)

	// Register as a managed runnable so the manager starts it AFTER the k8s
	// cache is ready. This prevents the "cache is not started" error when
	// Resolve() tries to read the gitlab-registry secret on startup.
	if err := mgr.Add(rec); err != nil {
		return fmt.Errorf("registering reconciler: %w", err)
	}

	return mgr.Start(context.Background())
}

func runGenerate(cfg *protoOperator.Config, log *zap.Logger) error {
	topo := cfg.GetTopology()
	if topo == nil || topo.GatewayNodes == 0 {
		return fmt.Errorf("topology.gateway_nodes must be set in GENERATE_ONLY mode")
	}

	tenantOwned, err := TenantOwnedServices()
	if err != nil {
		return fmt.Errorf("reading tenant-owned services from registry: %w", err)
	}
	pinnedTenant, err := PinnedTenantServices()
	if err != nil {
		return fmt.Errorf("reading pinned-tenant services from registry: %w", err)
	}
	g := helm.New(CurrentSeries(), CurrentMajorMinor(), nil, tenantOwned, pinnedTenant)
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
	fmt.Printf("Generated %d chart files → %s/schematic/\n", len(charts.Files), outDir)
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
  schematic-operator init ca
  schematic-operator init ca -c /path/to/config`,
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
	initClusterDev  bool
)

var initClusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Initialize cluster PKI and operator.yaml",
	Long: `Generate cluster PKI material and write operator.yaml.

Requires a root CA (from 'schematic-operator init ca' or an existing crypto.yaml).

Flags:
  --ca       Path to crypto.yaml containing the root CA (required if not in config dir).
  --fqdn     Public cluster FQDN (e.g. api.example.com). Required.
  --port     Gateway HTTPS port. Default 443.
  --type     Topology preset: small (default), medium, large.
  --gateways Number of gateway replicas (0 = auto from node count in IN_CLUSTER mode).

Example:
  schematic-operator init cluster --ca ../ca/crypto.yaml --fqdn api.example.com
  schematic-operator init cluster --ca ../ca/crypto.yaml --fqdn api.example.com --type medium --gateways 3`,
	RunE: runInitCluster,
}

func init() {
	initClusterCmd.Flags().StringVar(&initClusterFQDN, "fqdn", "", "public cluster FQDN (required)")
	initClusterCmd.Flags().IntVar(&initClusterPort, "port", 443, "gateway HTTPS port")
	initClusterCmd.Flags().StringVar(&initClusterCA, "ca", "", "path to root CA crypto.yaml")
	initClusterCmd.Flags().StringVar(&initClusterType, "type", "small", "topology preset: small, medium, large")
	initClusterCmd.Flags().IntVar(&initClusterGW, "gateways", 0, "gateway replicas (0 = auto from node count)")
	initClusterCmd.Flags().BoolVar(&initClusterDev, "dev", false, "enable dev mode (imagePullPolicy: Always, faster polling)")
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
		return fmt.Errorf("no root CA found — run 'schematic-operator init ca' first or provide --ca")
	}

	cfg.Fqdn = initClusterFQDN
	cfg.Port = int32(initClusterPort)
	cfg.Dev = initClusterDev
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
	if err := GenerateCrypto(cfg, nodeCount, 0, initClusterFQDN, "local.dev"); err != nil {
		return fmt.Errorf("generating cluster PKI: %w", err)
	}

	// Generate gateway external TLS cert (CERT_CHAIN mode, signed by cluster CA).
	if _, err := EnsureGatewayExternalTLS(cfg, initClusterFQDN); err != nil {
		return fmt.Errorf("generating gateway external TLS: %w", err)
	}

	// Write services/ — idempotent per file (skips existing files).
	fmt.Println("Writing services/...")
	if err := WriteServices(dir); err != nil {
		return err
	}

	if err := SaveSettings(dir, cfg); err != nil {
		return err
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		return err
	}

	// Ensure kernel.yaml has delegates for all deployed services.
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		return fmt.Errorf("ensuring service delegates: %w", err)
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
	fmt.Printf("  stratum:    %d (writers)\n", topo.StratumNodes)
	if topo.GetStratumSearchNodes() > 0 {
		fmt.Printf("  stratum:    %d (search)\n", topo.GetStratumSearchNodes())
	}
	fmt.Printf("  saml:       %s\n", samlStatus)
	fmt.Printf("  services/   editable service configs\n")
	fmt.Printf("  ca.pem      CA cert for host trust\n")
	fmt.Printf("\nEdit services/*.yaml, then run:\n")
	fmt.Printf("  schematic-operator generate\n")
	return nil
}
