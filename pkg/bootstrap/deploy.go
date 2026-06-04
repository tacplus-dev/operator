package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/libs/host"
	protoOpa "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/libs/opalib"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/libs/x509"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/kernel"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/operator"
)

// DeployConfigs holds all generated per-service configurations.
type DeployConfigs struct {
	CACertPEM         []byte
	BootstrapNames    []string
	BootstrapYAMLs    [][]byte
	SentinelConfig    []byte
	GatewayConfig     []byte
	EventbusConfig    []byte
	OauthConfig       []byte
	MsggatewayConfig  []byte
	OpaConfig         []byte
	AnthropicConfig   []byte
	OpenAIConfig      []byte
	SamlConfig        []byte
	StratumConfig     []byte
	ArgusConfig       []byte
	TimedbConfig      []byte
	GraphdbConfig     []byte
	ColddbConfig      []byte
	LetsencryptConfig []byte
	GatewayOpts       GatewayOptions
	Cfg               *protoOperator.Config
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
		fields["listen_address"] = map[string]any{"host": "0.0.0.0", "port": int64(8443)}
		fields["logger"] = map[string]any{"level": "LEVEL_DEBUG", "output_paths": []string{"stdout"}}
		if extra != nil {
			extra(fields)
		}
		return yaml.Marshal(fields)
	}

	authRules := func(m map[string]any) {
		m["max_request_size"] = int64(10 * 1024 * 1024)
		m["request_timeout_seconds"] = int64(30)
		m["shutdown_timeout_seconds"] = int64(30)
		m["auth_rules"] = map[string]any{
			"1": map[string]any{"action": "ACTION_PERMIT", "issuer": Issuer(cfg), "method": "*"},
		}
	}

	// mergeServiceYAML returns a buildSvc-extra lambda that overlays the
	// contents of services/<file> (if present) onto the generated config
	// fields. Used to plumb per-service settings (e.g. anthropic
	// federation_rule_id) authored in the deploy bundle through to the
	// service's runtime Config. Missing or unparseable file is silently
	// ignored — these yaml overlays are optional.
	mergeServiceYAML := func(file string) func(map[string]any) {
		return func(m map[string]any) {
			data, err := os.ReadFile(filepath.Join(cfgDir, "services", file))
			if err != nil {
				return
			}
			var src map[string]any
			if err := yaml.Unmarshal(data, &src); err != nil {
				return
			}
			for k, v := range src {
				m[k] = v
			}
		}
	}

	sentinelConfigYAML, err := buildSvc("sentinel", func(m map[string]any) {
		m["max_request_size"] = int64(10 * 1024 * 1024)
		m["request_timeout_seconds"] = int64(30)
		m["shutdown_timeout_seconds"] = int64(30)
		m["auth_rules"] = map[string]any{
			"1": map[string]any{"action": "ACTION_PERMIT", "issuer": Issuer(cfg), "method": "*"},
		}
	})
	if err != nil {
		return nil, err
	}

	gatewayKey, err := LoadPrivateKey(filepath.Join(cfgDir, "creds", "gateway"))
	if err != nil {
		return nil, fmt.Errorf("loading gateway key: %w", err)
	}
	gatewayOpts := LoadGatewayOptions(filepath.Join(cfgDir, "services"))
	if cfg.GetTls() != nil && cfg.GetTls().GetLetsencrypt() {
		gatewayOpts.LetsEncrypt = true
	}
	if cfg.Fqdn != "" {
		gatewayOpts.Hostnames = []string{cfg.Fqdn}
	}
	gatewayConfigYAML, err := BuildGatewayServiceConfig(gatewayKey, trustedCAs, gatewayOpts, cfg.SpiffePrefix, kernelEndpoints...)
	if err != nil {
		return nil, fmt.Errorf("building gateway config: %w", err)
	}
	opt := cfg.GetOptionalServices()

	oauthConfigYAML, err := buildSvc("oauth", nil)
	if err != nil {
		return nil, err
	}

	eventbusConfigYAML, err := buildSvc("eventbus", func(m map[string]any) {
		m["cluster_seed"] = "cluster"
	})
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

	opaKey, err := LoadPrivateKey(filepath.Join(cfgDir, "creds", "opa"))
	if err != nil {
		return nil, fmt.Errorf("loading opa key: %w", err)
	}
	opaFields, err := BuildBackendFields(opaKey, trustedCAs, kernelEndpoints, cfg.SpiffePrefix)
	if err != nil {
		return nil, fmt.Errorf("building opa config: %w", err)
	}
	opaFields["listen_address"] = map[string]any{"host": "0.0.0.0", "port": int64(8443)}
	opaFields["logger"] = map[string]any{"level": "LEVEL_DEBUG", "output_paths": []string{"stdout"}}

	btDir := BackendTokenPolicyDir(cfgDir)
	if _, err := os.Stat(btDir); err == nil {
		btPolicy, err := LoadOPAPolicy(btDir, "data.backend_token.eval")
		if err != nil {
			return nil, fmt.Errorf("loading OPA backend token policy: %w", err)
		}
		btYAML, err := protoOpa.OpaPolicyToYAML(btPolicy)
		if err != nil {
			return nil, fmt.Errorf("marshaling backend token policy: %w", err)
		}
		var btMap map[string]any
		if err := yaml.Unmarshal(btYAML, &btMap); err != nil {
			return nil, fmt.Errorf("converting backend token policy: %w", err)
		}
		opaFields["backend_token_policy"] = btMap
	}

	// Load OPA entitlements policy from services/opa/ if present.
	epDir := OPAPolicyDir(cfgDir)
	if _, err := os.Stat(epDir); err == nil {
		epPolicy, err := LoadOPAPolicy(epDir, "data.entitlements.eval")
		if err != nil {
			return nil, fmt.Errorf("loading OPA entitlements policy: %w", err)
		}
		epYAML, err := protoOpa.OpaPolicyToYAML(epPolicy)
		if err != nil {
			return nil, fmt.Errorf("marshaling entitlements policy: %w", err)
		}
		var epMap map[string]any
		if err := yaml.Unmarshal(epYAML, &epMap); err != nil {
			return nil, fmt.Errorf("converting entitlements policy: %w", err)
		}
		opaFields["entitlements_policy"] = epMap
	}

	// Load OPA proxy policy from services/opa-proxy-policy/ if present.
	ppDir := ProxyPolicyDir(cfgDir)
	if _, err := os.Stat(ppDir); err == nil {
		ppPolicy, err := LoadOPAPolicy(ppDir, "data.proxypolicy.eval")
		if err != nil {
			return nil, fmt.Errorf("loading OPA proxy policy: %w", err)
		}
		ppYAML, err := protoOpa.OpaPolicyToYAML(ppPolicy)
		if err != nil {
			return nil, fmt.Errorf("marshaling proxy policy: %w", err)
		}
		var ppMap map[string]any
		if err := yaml.Unmarshal(ppYAML, &ppMap); err != nil {
			return nil, fmt.Errorf("converting proxy policy: %w", err)
		}
		opaFields["proxy_policy"] = ppMap
	}
	opaConfigYAML, err := yaml.Marshal(opaFields)
	if err != nil {
		return nil, fmt.Errorf("marshaling opa config: %w", err)
	}

	anthropicConfigYAML, err := buildSvc("anthropic", mergeServiceYAML("anthropic.yaml"))
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

	stratumConfigYAML, err := buildSvc("stratum", authRules)
	if err != nil {
		return nil, err
	}

	argusConfigYAML, err := buildSvc("argus", nil)
	if err != nil {
		return nil, err
	}

	timedbConfigYAML, err := buildSvc("timedb", authRules)
	if err != nil {
		return nil, err
	}

	graphdbConfigYAML, err := buildSvc("graphdb", authRules)
	if err != nil {
		return nil, err
	}

	var colddbConfigYAML []byte
	if t.ColddbNodes > 0 {
		colddbConfigYAML, err = buildSvc("colddb", authRules)
		if err != nil {
			return nil, err
		}
	}

	var letsencryptConfigYAML []byte
	if cfg.GetTls().GetLetsencrypt() {
		letsencryptConfigYAML, err = buildSvc("letsencrypt", func(m map[string]any) {
			m["fqdn"] = cfg.Fqdn
			m["hostnames"] = []string{cfg.Fqdn}
			m["http_listener"] = map[string]any{"host": "0.0.0.0", "port": int64(8080)}
			m["renewal_check_interval_seconds"] = int64(3600)
		})
		if err != nil {
			return nil, err
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
		CACertPEM:            caCertPEM,
		BootstrapNames:       bootstrapNames,
		BootstrapYAMLs:       bootstrapYAMLs,
		SentinelConfig:       sentinelConfigYAML,
		GatewayConfig:        gatewayConfigYAML,
		EventbusConfig:       eventbusConfigYAML,
		OauthConfig:          oauthConfigYAML,
		MsggatewayConfig:     msggatewayConfigYAML,
		OpaConfig:            opaConfigYAML,
		AnthropicConfig:      anthropicConfigYAML,
		OpenAIConfig:         openaiConfigYAML,
		SamlConfig:           samlConfigYAML,
		StratumConfig:        stratumConfigYAML,
		ArgusConfig:          argusConfigYAML,
		TimedbConfig:         timedbConfigYAML,
		GraphdbConfig:        graphdbConfigYAML,
		ColddbConfig:         colddbConfigYAML,
		LetsencryptConfig:    letsencryptConfigYAML,
		GatewayOpts:          gatewayOpts,
		Cfg:                  cfg,
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
