package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/libs/host"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/libs/x509"
	protoEventbus "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/eventbus"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

// DeployConfigs holds all generated per-service configurations.
type DeployConfigs struct {
	CACertPEM                []byte
	BootstrapNames           []string
	BootstrapYAMLs           [][]byte
	SentinelConfig           []byte
	GatewayConfig            []byte
	EventbusConfig           []byte
	OauthConfig              []byte
	MsggatewayConfig         []byte
	OpaConfig                []byte
	AnthropicConfig          []byte
	OpenAIConfig             []byte
	SamlConfig               []byte
	StratumConfig            []byte
	ArgusConfig              []byte
	TimedbConfig             []byte
	GraphdbConfig            []byte
	ColddbConfig             []byte
	LetsencryptConfig        []byte
	LetsencryptServiceConfig []byte
	GatewayOpts              GatewayOptions
	Cfg                      *protoOperator.Config
}

// BuildDeployConfigs loads config from cfgDir, generates crypto if needed, builds all
// per-service configs, and returns them in a DeployConfigs struct.
func BuildDeployConfigs(cfgDir string) (*DeployConfigs, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	if cfg.Topology == nil {
		return nil, fmt.Errorf("no topology configured — run 'cavalier-operator init cluster' first")
	}

	deployList, err := LoadDeployList(cfgDir)
	if err != nil {
		return nil, err
	}
	FilterTopology(cfg.Topology, deployList)

	t := cfg.Topology

	nodeCount := int(t.KernelNodes)

	// Generate crypto if the cluster has no TLS certs yet.
	if cfg.Pki == nil || len(cfg.Pki.NodeTls) == 0 {
		if err := GenerateCrypto(cfg, nodeCount, 0, cfg.Fqdn); err != nil {
			return nil, fmt.Errorf("generating crypto material: %w", err)
		}
		if err := SaveCrypto(cfgDir, cfg); err != nil {
			return nil, fmt.Errorf("saving crypto: %w", err)
		}
	}

	caCertPEM, err := RootCACertPEM(cfg)
	if err != nil {
		return nil, fmt.Errorf("extracting root CA PEM: %w", err)
	}
	// Write ca.pem for tools that need it. Non-fatal: the reconciler runs in a
	// read-only config dir so the write may fail, but the cert is still returned
	// in DeployConfigs.CACertPEM for callers that need it.
	_ = os.WriteFile(filepath.Join(cfgDir, "ca.pem"), caCertPEM, 0644)

	kernelConfig, err := BuildKernelConfigProto(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("building kernel config: %w", err)
	}

	if cfg.Pki.ClusterCa != nil {
		kernelConfig.ClusterCa = cfg.Pki.ClusterCa.Chain
	}

	// Root-admin bootstrap cred_pairs come from operator.yaml. Render
	// them into kernel's config so kernel can authenticate TenantAdmin
	// RPCs (CreateTenant, DeleteTenant, ListTenants) after boot. The
	// operator itself does not store the private keys — they live with
	// the cluster/root admin / CI pipeline.
	if len(cfg.RootCredPairs) > 0 {
		kernelConfig.RootCredPairs = cfg.RootCredPairs
	}

	// Assemble kernel trusted CAs: root + cluster + signing CAs.
	// Kernel needs these for S2S TLS verification (e.g. kernel → OPA).
	if cfg.Pki.RootCa != nil {
		kernelConfig.TrustedCas = append(kernelConfig.TrustedCas, cfg.Pki.RootCa.Chain...)
	}
	if cfg.Pki.ClusterCa != nil {
		kernelConfig.TrustedCas = append(kernelConfig.TrustedCas, cfg.Pki.ClusterCa.Chain...)
	}
	for _, sc := range cfg.Pki.NodeSigningCa {
		if len(sc.Chain) > 0 {
			kernelConfig.TrustedCas = append(kernelConfig.TrustedCas, sc.Chain[0])
		}
	}

	if kernelConfig.Configs != nil && kernelConfig.Configs.EventbusConfig != nil {
		if kernelConfig.Configs.EventbusConfig.NodeRole == protoEventbus.KafkaNodeRole_KAFKA_ROLE_UNDEFINED {
			kernelConfig.Configs.EventbusConfig.NodeRole = protoEventbus.KafkaNodeRole_KAFKA_ROLE_COMBINED
		}
		if kernelConfig.Configs.EventbusConfig.ClusterSeed == "" {
			kernelConfig.Configs.EventbusConfig.ClusterSeed = "cluster"
		}
	}

	// Assemble trusted CAs: root + cluster + per-node signing CAs.
	var trustedCAs []*protoX509.X509Cert
	if cfg.Pki.RootCa != nil {
		trustedCAs = append(trustedCAs, cfg.Pki.RootCa.Chain...)
	}
	if cfg.Pki.ClusterCa != nil {
		trustedCAs = append(trustedCAs, cfg.Pki.ClusterCa.Chain...)
	}
	for _, sc := range cfg.Pki.NodeSigningCa {
		if len(sc.Chain) > 0 {
			trustedCAs = append(trustedCAs, sc.Chain[0])
		}
	}

	kernelEndpoints := make([]*protoHost.Host, nodeCount)
	for i := range nodeCount {
		kernelEndpoints[i] = &protoHost.Host{Host: fmt.Sprintf("kernel-%d.kernel", i), Port: int32(protoKernel.DefaultListenerPort)}
	}

	buildSvc := func(name string, extra func(map[string]any)) ([]byte, error) {
		key, err := LoadPrivateKey(filepath.Join(cfgDir, "creds", name))
		if err != nil {
			return nil, fmt.Errorf("loading %s key: %w", name, err)
		}
		fields, err := BuildBackendFields(key, trustedCAs, kernelEndpoints, cfg.SpiffePrefix)
		if err != nil {
			return nil, fmt.Errorf("building %s config: %w", name, err)
		}
		applyServiceDefaults(fields)
		if extra != nil {
			extra(fields)
		}
		return yaml.Marshal(fields)
	}

	sentinelConfigYAML, err := buildSvc("sentinel", nil)
	if err != nil {
		return nil, err
	}

	gatewayOpts := LoadGatewayOptions(filepath.Join(cfgDir, "services"))
	if cfg.GetTls() != nil && cfg.GetTls().GetLetsencrypt() {
		gatewayOpts.LetsEncrypt = true
	}
	if cfg.Fqdn != "" {
		gatewayOpts.Hostnames = []string{cfg.Fqdn}
	}
	gatewayConfigYAML, err := buildSvc("gateway", nil)
	if err != nil {
		return nil, fmt.Errorf("building gateway config: %w", err)
	}
	opt := cfg.GetOptionalServices()

	oauthConfigYAML, err := buildSvc("oauth", nil)
	if err != nil {
		return nil, err
	}

	eventbusConfigYAML, err := buildSvc("eventbus", nil)
	if err != nil {
		return nil, err
	}

	var msggatewayConfigYAML []byte
	if t.MsggatewayNodes > 0 {
		msggatewayConfigYAML, err = buildSvc("msggateway", nil)
		if err != nil {
			return nil, err
		}
	}

	opaConfigYAML, err := buildSvc("opa", nil)
	if err != nil {
		return nil, err
	}

	anthropicConfigYAML, err := buildSvc("anthropic", nil)
	if err != nil {
		return nil, err
	}

	openaiConfigYAML, err := buildSvc("openai", nil)
	if err != nil {
		return nil, err
	}

	var samlConfigYAML []byte
	if opt.GetSaml() {
		samlConfigYAML, err = buildSvc("saml", nil)
		if err != nil {
			return nil, err
		}
	}

	stratumConfigYAML, err := buildSvc("stratum", nil)
	if err != nil {
		return nil, err
	}

	argusConfigYAML, err := buildSvc("argus", nil)
	if err != nil {
		return nil, err
	}

	timedbConfigYAML, err := buildSvc("timedb", nil)
	if err != nil {
		return nil, err
	}

	graphdbConfigYAML, err := buildSvc("graphdb", nil)
	if err != nil {
		return nil, err
	}

	var colddbConfigYAML []byte
	if t.ColddbNodes > 0 {
		colddbConfigYAML, err = buildSvc("colddb", nil)
		if err != nil {
			return nil, err
		}
	}

	var letsencryptConfigYAML []byte
	var letsencryptServiceConfigYAML []byte
	if cfg.GetTls().GetLetsencrypt() {
		letsencryptConfigYAML, err = buildSvc("letsencrypt", nil)
		if err != nil {
			return nil, err
		}
		letsencryptServiceConfigYAML, err = yaml.Marshal(map[string]any{
			"fqdn":                           cfg.Fqdn,
			"hostnames":                      []string{cfg.Fqdn},
			"http_listener":                  map[string]any{"host": "0.0.0.0", "port": int64(8080)},
			"renewal_check_interval_seconds": int64(3600),
		})
		if err != nil {
			return nil, fmt.Errorf("marshaling letsencrypt service config: %w", err)
		}
	}

	// Per-node kernel bootstrap configs.
	members := make([]*protoHost.Host, nodeCount)
	for i := range nodeCount {
		members[i] = &protoHost.Host{Host: fmt.Sprintf("kernel-%d.kernel", i), Port: int32(protoKernel.DefaultHazelcastListenerPort)}
	}

	var bootstrapYAMLs [][]byte
	var bootstrapNames []string
	for i := range nodeCount {
		nodeConfig := proto.Clone(kernelConfig).(*protoKernel.Config)
		nodeConfig.ClusterName = "kernel"
		nodeConfig.Listener = &protoHost.Host{Host: "0.0.0.0", Port: int32(protoKernel.DefaultListenerPort)}
		nodeConfig.Members = members
		nodeConfig.NodeCount = int32(nodeCount)
		nodeConfig.PartitionCount = 271
		nodeConfig.HazelcastListener = &protoHost.Host{Host: "0.0.0.0", Port: int32(protoKernel.DefaultHazelcastListenerPort)}
		nodeConfig.Tls = cfg.Pki.NodeTls[i]
		if i < len(cfg.Pki.NodeSigningCa) {
			nodeConfig.SigningCa = cfg.Pki.NodeSigningCa[i]
		}
		// Override persistence_dir to match the Helm PVC mount path (/app/data).
		// The default "data/kernel" resolves to /data/kernel in the container
		// (working dir is /), which is not the mounted volume.
		nodeConfig.PersistenceDir = "/app/data/kernel"

		bsYAML, err := protoKernel.ConfigToYAML(nodeConfig)
		if err != nil {
			return nil, fmt.Errorf("marshaling bootstrap config %d: %w", i, err)
		}
		bootstrapNames = append(bootstrapNames, fmt.Sprintf("bootstrap%d", i))
		bootstrapYAMLs = append(bootstrapYAMLs, bsYAML)
	}

	return &DeployConfigs{
		CACertPEM:                caCertPEM,
		BootstrapNames:           bootstrapNames,
		BootstrapYAMLs:           bootstrapYAMLs,
		SentinelConfig:           sentinelConfigYAML,
		GatewayConfig:            gatewayConfigYAML,
		EventbusConfig:           eventbusConfigYAML,
		OauthConfig:              oauthConfigYAML,
		MsggatewayConfig:         msggatewayConfigYAML,
		OpaConfig:                opaConfigYAML,
		AnthropicConfig:          anthropicConfigYAML,
		OpenAIConfig:             openaiConfigYAML,
		SamlConfig:               samlConfigYAML,
		StratumConfig:            stratumConfigYAML,
		ArgusConfig:              argusConfigYAML,
		TimedbConfig:             timedbConfigYAML,
		GraphdbConfig:            graphdbConfigYAML,
		ColddbConfig:             colddbConfigYAML,
		LetsencryptConfig:        letsencryptConfigYAML,
		LetsencryptServiceConfig: letsencryptServiceConfigYAML,
		GatewayOpts:              gatewayOpts,
		Cfg:                      cfg,
	}, nil
}

// FilterTopology zeros out node counts for services not in the allowed set.
// No-op if allowed is nil.
func FilterTopology(t *protoOperator.Topology, allowed map[string]bool) {
	if allowed == nil {
		return
	}
	check := func(name string, count *int32) {
		if !allowed[name] {
			*count = 0
		}
	}
	check("kernel", &t.KernelNodes)
	check("sentinel", &t.SentinelNodes)
	check("eventbus", &t.EventbusNodes)
	check("gateway", &t.GatewayNodes)
	check("oauth", &t.OauthNodes)
	check("msggateway", &t.MsggatewayNodes)
	check("opa", &t.OpaNodes)
	check("timedb", &t.TimedbNodes)
	check("graphdb", &t.GraphdbNodes)
	check("colddb", &t.ColddbNodes)
	check("argus", &t.ArgusNodes)
	check("stratum", &t.StratumNodes)
	check("saml", &t.SamlNodes)
}

// ServiceConfigValues converts a DeployConfigs into a Helm values map.
// Each entry is keyed by service name and contains {"config": yaml-string},
// matching the {{ .Values.<svc>.config }} field in the chart templates.
func ServiceConfigValues(dc *DeployConfigs) map[string]interface{} {
	vals := map[string]interface{}{}

	// kernel — one config per replica (each has its own TLS cert and node index)
	for i, bsYAML := range dc.BootstrapYAMLs {
		vals[fmt.Sprintf("kernel%d", i)] = map[string]interface{}{"config": string(bsYAML)}
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
			vals[pair.name] = map[string]interface{}{"config": string(pair.data)}
		}
	}
	if len(dc.LetsencryptServiceConfig) > 0 {
		le, ok := vals["letsencrypt"].(map[string]interface{})
		if !ok {
			le = map[string]interface{}{}
			vals["letsencrypt"] = le
		}
		le["serviceConfig"] = string(dc.LetsencryptServiceConfig)
	}

	return vals
}

// NewServiceConfigProvider returns a reconciler.ServiceConfigProvider that calls
// BuildDeployConfigs(cfgDir) on each invocation. If cfgDir is empty or the
// config directory is not yet fully initialized, it returns nil values (no override).
func NewServiceConfigProvider(cfgDir string) func() (map[string]interface{}, error) {
	return func() (map[string]interface{}, error) {
		if cfgDir == "" {
			return nil, nil
		}
		dc, err := BuildDeployConfigs(cfgDir)
		if err != nil {
			return nil, err
		}
		return ServiceConfigValues(dc), nil
	}
}
