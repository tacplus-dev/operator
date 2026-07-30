package bootstrap

import (
	"fmt"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

// --- generate ---

var (
	generateOutputDir string
	generateGateways  int
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate Helm charts to disk",
	Long: `Generate the cavalier Helm chart for the configured cluster and write it to disk.

Does not require or communicate with a Kubernetes cluster. Use this to produce
charts for GitOps workflows or for inspection before applying.

The --gateways flag overrides topology.gateway_nodes for this run only.

Examples:
  cavalier-operator generate
  cavalier-operator generate --output ./release/charts
  cavalier-operator generate --gateways 3`,
	RunE: runGenerateCmd,
}

func init() {
	generateCmd.Flags().StringVarP(&generateOutputDir, "output", "o", "", "output directory (default: operator.yaml output_dir or 'charts')")
	generateCmd.Flags().IntVar(&generateGateways, "gateways", 0, "override gateway replica count for this run")
}

func runGenerateCmd(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()

	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}

	if generateGateways > 0 {
		if cfg.Topology == nil {
			cfg.Topology = &protoOperator.Topology{}
		}
		cfg.Topology.GatewayNodes = int32(generateGateways)
	}

	if cfg.Topology == nil || cfg.Topology.GatewayNodes == 0 {
		return fmt.Errorf("gateway_nodes must be set — use --gateways or set topology.gateway_nodes in operator.yaml")
	}

	outDir := generateOutputDir
	if outDir == "" {
		outDir = cfg.OutputDir
	}
	if outDir == "" {
		outDir = "charts"
	}

	g := helm.New(CurrentSeries(), CurrentMajorMinor(), nil)
	charts, err := g.Generate(cfg, 0)
	if err != nil {
		return err
	}
	if err := g.WriteToDisk(charts, outDir); err != nil {
		return err
	}

	fmt.Printf("Generated %d chart files → %s/cavalier/\n", len(charts.Files), outDir)
	return nil
}

// --- apply ---

var (
	applyGateways  int
	applyDryRun    bool
	applyBundleFile string
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Generate charts and apply to the current Kubernetes context (one-shot)",
	Long: `Generate the cavalier Helm chart and apply it to the current Kubernetes cluster.
Unlike the in-cluster reconciler, this runs once and exits.

With --bundle, reads a pre-built bundle file instead of the config directory.
The bundle embeds all rendered service configs — no config directory needed.

Examples:
  cavalier-operator apply
  cavalier-operator apply --bundle cavalier-bundle.yaml
  cavalier-operator apply --gateways 3
  cavalier-operator apply --dry-run`,
	RunE: runApply,
}

func init() {
	applyCmd.Flags().IntVar(&applyGateways, "gateways", 0, "override gateway replica count")
	applyCmd.Flags().BoolVar(&applyDryRun, "dry-run", false, "print manifests without applying")
	applyCmd.Flags().StringVar(&applyBundleFile, "bundle", "", "deploy from a pre-built bundle file")
}

func runApply(cmd *cobra.Command, args []string) error {
	var cfg *protoOperator.Config
	var svcValues map[string]interface{}

	if applyBundleFile != "" {
		b, err := LoadBundle(applyBundleFile)
		if err != nil {
			return err
		}
		cfg, err = BundleConfig(b)
		if err != nil {
			return err
		}
		svcValues = BundleServiceValues(b)
	} else {
		dir := resolveConfigDir()
		var err error
		cfg, err = LoadConfig(dir)
		if err != nil {
			return err
		}
		if err := ValidateConfig(cfg); err != nil {
			return err
		}
		dc, err := BuildDeployConfigs(dir)
		if err != nil {
			return fmt.Errorf("building deploy configs: %w", err)
		}
		svcValues = ServiceConfigValues(dc)
	}

	if applyGateways > 0 {
		if cfg.Topology == nil {
			cfg.Topology = &protoOperator.Topology{}
		}
		cfg.Topology.GatewayNodes = int32(applyGateways)
	}

	ns := cfg.GetNamespace()
	if ns == "" {
		ns = "cavalier"
	}

	g := helm.New(CurrentSeries(), CurrentMajorMinor(), nil)
	charts, err := g.Generate(cfg, 0)
	if err != nil {
		return err
	}

	if applyDryRun {
		buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
		for name, data := range charts.Files {
			buffered = append(buffered, &helmloader.BufferedFile{
				Name: name,
				Data: data,
			})
		}
		ch, err := helmloader.LoadFiles(buffered)
		if err != nil {
			return fmt.Errorf("loading helm chart for dry-run: %w", err)
		}
		if svcValues == nil {
			svcValues = map[string]interface{}{}
		}
		install := action.NewInstall(new(action.Configuration))
		install.ReleaseName = "cavalier"
		install.Namespace = ns
		install.DryRun = true
		install.ClientOnly = true
		rel, err := install.Run(ch, svcValues)
		if err != nil {
			return fmt.Errorf("helm dry-run: %w", err)
		}
		fmt.Print(rel.Manifest)
		return nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}

	getter := &cliGetter{cfg: restCfg, namespace: ns}

	actionCfg := new(action.Configuration)
	if err := actionCfg.Init(getter, ns, "secret", func(format string, v ...interface{}) {
		fmt.Printf(format+"\n", v...)
	}); err != nil {
		return fmt.Errorf("initialising helm action config: %w", err)
	}

	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{
			Name: name,
			Data: data,
		})
	}

	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		return fmt.Errorf("loading helm chart from memory: %w", err)
	}

	histAction := action.NewHistory(actionCfg)
	histAction.Max = 1
	_, histErr := histAction.Run("cavalier")

	switch {
	case histErr == helmdriver.ErrReleaseNotFound:
		relName, err := cliHelmInstall(actionCfg, ch, ns, svcValues)
		if err != nil {
			return err
		}
		fmt.Printf("Installed release %q (%d chart files)\n", relName, len(charts.Files))
	case histErr != nil:
		return fmt.Errorf("querying helm release history: %w", histErr)
	default:
		relName, err := cliHelmUpgrade(actionCfg, ch, svcValues)
		if err != nil {
			return err
		}
		fmt.Printf("Upgraded release %q (%d chart files)\n", relName, len(charts.Files))
	}

	return nil
}

// cliGetter implements action.RESTClientGetter using a rest.Config loaded from
// the local kubeconfig (or in-cluster environment).
type cliGetter struct {
	cfg       *rest.Config
	namespace string
}

func (g *cliGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	cfg := clientcmdapi.NewConfig()
	cluster := &clientcmdapi.Cluster{
		Server:                   g.cfg.Host,
		CertificateAuthorityData: g.cfg.TLSClientConfig.CAData,
		InsecureSkipTLSVerify:    g.cfg.TLSClientConfig.Insecure,
	}
	cfg.Clusters["cluster"] = cluster
	authInfo := &clientcmdapi.AuthInfo{
		Token:                 g.cfg.BearerToken,
		ClientCertificateData: g.cfg.TLSClientConfig.CertData,
		ClientKeyData:         g.cfg.TLSClientConfig.KeyData,
	}
	cfg.AuthInfos["user"] = authInfo
	cfg.Contexts["ctx"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "user", Namespace: g.namespace}
	cfg.CurrentContext = "ctx"
	return clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{})
}

func (g *cliGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(rest.CopyConfig(g.cfg))
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (g *cliGetter) ToRESTMapper() (apimeta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(g.cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	return mapper, nil
}

func (g *cliGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.cfg), nil
}

func cliHelmInstall(cfg *action.Configuration, ch *helmchart.Chart, ns string, vals map[string]interface{}) (string, error) {
	install := action.NewInstall(cfg)
	install.ReleaseName = "cavalier"
	install.Namespace = ns
	install.CreateNamespace = true
	install.Wait = false

	if vals == nil {
		vals = map[string]interface{}{}
	}
	rel, err := install.Run(ch, vals)
	if err != nil {
		return "", fmt.Errorf("helm install: %w", err)
	}
	return rel.Name, nil
}

func cliHelmUpgrade(cfg *action.Configuration, ch *helmchart.Chart, vals map[string]interface{}) (string, error) {
	upgrade := action.NewUpgrade(cfg)
	upgrade.Wait = false

	if vals == nil {
		vals = map[string]interface{}{}
	}
	rel, err := upgrade.Run("cavalier", ch, vals)
	if err != nil {
		return "", fmt.Errorf("helm upgrade: %w", err)
	}
	return rel.Name, nil
}

// --- set-version ---

var setVersionCmd = &cobra.Command{
	Use:   "set-version <service-version>",
	Short: "Update the service_version in operator.yaml (e.g. v1.2.x-dev, v1.2.11)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSetVersion,
}

func runSetVersion(cmd *cobra.Command, args []string) error {
	return fmt.Errorf("set-version is deprecated — use 'set-patch' and 'set-dev' instead")
}

// --- set-mode ---

var setModeCmd = &cobra.Command{
	Use:   "set-mode <in-cluster|generate-only>",
	Short: "Switch operator mode in operator.yaml",
	Args:  cobra.ExactArgs(1),
	RunE:  runSetMode,
}

func runSetMode(cmd *cobra.Command, args []string) error {
	dir := resolveConfigDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		return err
	}

	switch args[0] {
	case "in-cluster":
		cfg.Mode = protoOperator.OperatorMode_IN_CLUSTER
	case "generate-only":
		cfg.Mode = protoOperator.OperatorMode_GENERATE_ONLY
	default:
		return fmt.Errorf("unknown mode %q — must be 'in-cluster' or 'generate-only'", args[0])
	}

	if err := SaveSettings(dir, cfg); err != nil {
		return err
	}
	fmt.Printf("Mode set: %s\n", args[0])
	return nil
}

// --- helpers ---

func resolveConfigDirAuto() string {
	return ResolveConfigDirSmart()
}
