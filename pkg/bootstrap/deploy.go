package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"gopkg.in/yaml.v3"

	wrapperX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/x509"
	protoServicesConfig "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/generated/services_config"
	protoBlobstore "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/blobstore"
	protoEventbus "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/eventbus"
	protoGateway "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/gateway"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/host"
	protoX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/x509"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/reconciler"
)

// DeployConfigs holds all generated per-service configurations.
type DeployConfigs struct {
	CACertPEM          []byte
	BootstrapNames     []string
	BootstrapYAMLs     [][]byte
	SentinelConfig     []byte
	EchoConfig         []byte
	MessagingConfig    []byte
	MailboxConfig      []byte
	PersonalauthConfig []byte
	// AppConfigs holds bootstrap YAML for the application's own services, keyed
	// by service name. Platform services each have a named field above because
	// the platform's service set is fixed; an app's is not known until its
	// config declares it, so those cannot be fields.
	AppConfigs               map[string][]byte
	GatewayConfig            []byte
	EventbusConfig           []byte
	OauthConfig              []byte
	OpaConfig                []byte
	SamlConfig               []byte
	StratumConfig            []byte
	KmakerConfig             []byte
	LetsencryptConfig        []byte
	LetsencryptServiceConfig []byte
	GatewayOpts              GatewayOptions
	Cfg                      *protoOperator.Config

	// OperatorBootstrapConfig is the operator's OWN chassis grpcconfig.Config —
	// the same shape every other service gets, built from the same kernel
	// endpoints, trusted CAs and SPIFFE fields. Nothing else generates this file
	// for it: it is the thing that generates everyone else's. Carries no private
	// key (bootstrap is a projected k8s token). The operator no longer uses its
	// kernel identity at runtime — the live opa config push it existed for was
	// retired 2026-09-24 (the bundle is the only config writer).
	OperatorBootstrapConfig []byte
}

// BuildDeployConfigs loads config from cfgDir, generates crypto if needed, builds all
// per-service configs, and returns them in a DeployConfigs struct.
func BuildDeployConfigs(cfgDir string) (*DeployConfigs, error) {
	return buildDeployConfigs(cfgDir, nil, nil)
}

func buildDeployConfigs(cfgDir string, pkiOverride *protoOperator.PKI, kmakerTLSOverride *protoX509.X509CertChain) (*DeployConfigs, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if pkiOverride != nil {
		pki, ok := proto.Clone(pkiOverride).(*protoOperator.PKI)
		if !ok {
			return nil, fmt.Errorf("cloning cached pki")
		}
		cfg.Pki = pki
	}
	if kmakerTLSOverride != nil {
		tls, ok := proto.Clone(kmakerTLSOverride).(*protoX509.X509CertChain)
		if !ok {
			return nil, fmt.Errorf("cloning cached kmaker serving tls")
		}
		if cfg.Kmaker == nil {
			cfg.Kmaker = &protoKmaker.Config{}
		}
		cfg.Kmaker.Tls = tls
	}

	if cfg.Topology == nil {
		return nil, fmt.Errorf("no topology configured — run 'schematic-operator init cluster' first")
	}

	deployList, err := LoadDeployList(cfgDir)
	if err != nil {
		return nil, err
	}
	FilterTopology(cfg.Topology, deployList)

	t := cfg.Topology

	nodeCount := int(t.KernelNodes)

	kernelConfig, err := BuildKernelConfigProto(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("building kernel config: %w", err)
	}
	protoKernel.ConfigDefaults(kernelConfig)
	// kernel.ConfigDefaults only defaults kernel's own scalar fields; it does
	// not recurse into Configs, the holder kernel re-seeds every service's
	// ConfigState from. Default each entry there too, or a service the bundle
	// does not mention boots against an all-zero config.
	if kernelConfig.Configs == nil {
		kernelConfig.Configs = &protoServicesConfig.Services{}
	}
	if err := ApplyServicesHolderDefaults(kernelConfig.Configs, DeployedKernelAuthServices(cfg)); err != nil {
		return nil, err
	}
	spiffeTrustDomain := kernelConfig.GetSpiffeTrustDomain()
	spiffeLocationPath := kernelConfig.GetLocationPath()
	kernelSpiffeURI, err := KernelServiceSpiffeURI(spiffeTrustDomain)
	if err != nil {
		return nil, err
	}

	// Generate or rotate crypto if the cluster has no TLS certs yet, or if
	// existing kernel certs predate the canonical SPIFFE service URI.
	cryptoUpdated := false
	if KernelTLSNeedsRegeneration(cfg, nodeCount, 0, kernelSpiffeURI) {
		if hasClusterCAKey(cfg) {
			if err := GenerateKernelTLS(cfg, nodeCount, 0, cfg.Fqdn, spiffeTrustDomain); err != nil {
				return nil, fmt.Errorf("regenerating kernel TLS material: %w", err)
			}
		} else if err := GenerateCrypto(cfg, nodeCount, 0, cfg.Fqdn, spiffeTrustDomain); err != nil {
			return nil, fmt.Errorf("generating crypto material: %w", err)
		}
		cryptoUpdated = true
	}
	if kmakerSigningCANeedsGeneration(cfg) {
		if err := ensureKmakerSigningCA(cfg); err != nil {
			return nil, fmt.Errorf("generating kmaker signing CA: %w", err)
		}
		cryptoUpdated = true
	}
	// Defer crypto save until after all crypto mutations (including gateway external TLS).

	caCertPEM, err := RootCACertPEM(cfg)
	if err != nil {
		return nil, fmt.Errorf("extracting root CA PEM: %w", err)
	}
	// Write ca.pem for tools that need it. Non-fatal: the reconciler runs in a
	// read-only config dir so the write may fail, but the cert is still returned
	// in DeployConfigs.CACertPEM for callers that need it.
	_ = os.WriteFile(filepath.Join(cfgDir, "ca.pem"), caCertPEM, 0644)

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
		fields, err := BuildBackendFields(trustedCAs, kernelEndpoints, spiffeTrustDomain, spiffeLocationPath)
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

	// echo takes the plain bootstrap shape -- no state, no S2S deps, no extras.
	echoConfigYAML, err := buildSvc("echo", nil)
	if err != nil {
		return nil, err
	}

	// messaging and mailbox take the same plain shape: their eventbus and
	// stratum access is s2s, routed through kernel by the generated
	// ServiceManager, so nothing extra belongs in the bootstrap config.
	messagingConfigYAML, err := buildSvc("messaging", nil)
	if err != nil {
		return nil, err
	}

	mailboxConfigYAML, err := buildSvc("mailbox", nil)
	if err != nil {
		return nil, err
	}

	// personalauth takes the plain shape too: its only dependency is kernel,
	// reached s2s through the generated ServiceManager.
	personalauthConfigYAML, err := buildSvc("personalauth", nil)
	if err != nil {
		return nil, err
	}

	// The application's own services take the same plain shape, plus the
	// shared llm_credential value for any service named in
	// llm_credential_services. A service that needs more (state, S2S deps) is
	// not expressible yet; see the helm generator's app-service block.
	llmCredentialTargets := map[string]bool{}
	for _, name := range cfg.GetLlmCredentialServices() {
		llmCredentialTargets[name] = true
	}
	// One config per DEPLOYED instance, not per service name: a tenant-owned
	// service's pods are named "<svc>-<tenant>" and each looks its config up
	// under that name. The config bytes are identical — nothing in a service's
	// bootstrap config names its tenant; kernel derives that from the delegate.
	tenantOwned, err := tenantOwnedServices()
	if err != nil {
		return nil, fmt.Errorf("reading tenant-owned services from registry: %w", err)
	}
	pinnedTenant, err := pinnedTenantServices()
	if err != nil {
		return nil, fmt.Errorf("reading pinned-tenant services from registry: %w", err)
	}
	appConfigs := map[string][]byte{}
	for _, name := range AppServiceNames(cfg) {
		var extra func(map[string]any)
		if cfg.GetLlmCredential() != "" && llmCredentialTargets[name] {
			extra = func(fields map[string]any) {
				fields["llm_credential"] = cfg.GetLlmCredential()
			}
		}
		y, err := buildSvc(name, extra)
		if err != nil {
			return nil, fmt.Errorf("building config for app service %q: %w", name, err)
		}
		for _, inst := range serviceInstances(cfg, name, tenantOwned, pinnedTenant) {
			appConfigs[inst.InstanceName()] = y
		}
	}

	gatewayOpts := LoadGatewayOptions(filepath.Join(cfgDir, "services"))
	if cfg.GetTls() != nil && cfg.GetTls().GetLetsencrypt() {
		gatewayOpts.LetsEncrypt = true
	}
	if cfg.Fqdn != "" {
		gatewayOpts.Hostnames = []string{cfg.Fqdn}
	}

	// Ensure gateway has a cluster-CA external TLS cert in BOTH modes.
	// CERT_CHAIN mode serves it for the life of the pod. LETSENCRYPT mode
	// serves it as the interim edge cert at startup: gateway owns port 80,
	// which answers the HTTP-01 challenge, so it cannot block on a real ACME
	// cert without deadlocking its own issuance (gateway d29e0f1). Its watcher
	// hot-swaps the public cert in once LetsEncryptState delivers one.
	if cfg.Fqdn != "" {
		gwTLSGenerated, err := EnsureGatewayExternalTLS(cfg, cfg.Fqdn)
		if err != nil {
			return nil, fmt.Errorf("ensuring gateway external TLS: %w", err)
		}
		if gwTLSGenerated {
			cryptoUpdated = true
		}
		certPEM, keyPEM, err := GatewayExternalTLSPEM(cfg)
		if err != nil {
			return nil, fmt.Errorf("extracting gateway external TLS PEM: %w", err)
		}
		gatewayOpts.ExternalCertPEM = certPEM
		gatewayOpts.ExternalKeyPEM = keyPEM
		if cfg.Pki.RootCa != nil {
			gatewayOpts.RootCAs = cfg.Pki.RootCa.Chain
		}
	}

	// Propagate operator-derived gateway TLS fields into kernel's services
	// holder so that kernel serves the complete gateway config at runtime.
	// BuildKernelConfigProto only loads the sparse services/gateway.yaml
	// (enable_tcp/enable_quic); the external TLS fields are computed here
	// from the operator's PKI and must be injected before kernel serializes.
	{
		if kernelConfig.Configs == nil {
			kernelConfig.Configs = &protoServicesConfig.Services{}
		}
		if kernelConfig.Configs.GatewayConfig == nil {
			kernelConfig.Configs.GatewayConfig = &protoGateway.Config{}
			protoGateway.ConfigDefaults(kernelConfig.Configs.GatewayConfig)
		}
		gwCfg := kernelConfig.Configs.GatewayConfig
		if gatewayOpts.LetsEncrypt {
			gwCfg.ExternalTlsMode = protoGateway.ExternalTlsMode_EXTERNAL_TLS_MODE_LETSENCRYPT
			// The gateway owns the edge's port 80 in this mode: its own
			// LoadBalancer carries 443 and 80, and the letsencrypt service is
			// no longer exposed outside the cluster at all. Both fields are
			// declared without a default (the answer depends on
			// external_tls_mode, which tacplus cannot express as a conditional
			// default), so the operator is what turns the listener on.
			gwCfg.HttpListener = &protoHost.Host{Host: "0.0.0.0", Port: gatewayHttpPort}
			gwCfg.AcmeBackend = &protoHost.Host{Host: letsencryptAcmeHost(cfg), Port: letsencryptAcmePort}
		} else {
			gwCfg.ExternalTlsMode = protoGateway.ExternalTlsMode_EXTERNAL_TLS_MODE_CERT_CHAIN
			gwCfg.HttpListener = nil
			gwCfg.AcmeBackend = nil
		}
		gwCfg.ExternalHostnames = gatewayOpts.Hostnames
		gwCfg.ExternalCertPem = gatewayOpts.ExternalCertPEM
		gwCfg.ExternalKeyPem = gatewayOpts.ExternalKeyPEM
		gwCfg.RootCa = gatewayOpts.RootCAs
	}

	// Seed blobstore's upload-token signing key into its holder entry.
	//
	// The key is GENERATED ONCE and kept in the operator's own config (PKI
	// .blobstore_token_key), because both pods have to hold the same one and a key
	// that changed on every deploy would invalidate every upload token in flight.
	// It reaches the pods only through this holder entry: kernel writes each entry
	// to that service's ConfigState boundary, which is where the generated chassis
	// takes a service's typed Config from — the same path the llm_credential seed
	// below uses, and for the same reason.
	//
	// The field is assert_required with no default, so blobstore is one of the
	// services whose holder entry the operator must fill rather than leave to
	// ConfigDefaults. Without this, kernel would re-seed blobstore's ConfigState
	// from an all-zero config on every boot and blobstore would crash-loop on its
	// own validation, which is exactly the failure mailbox, messaging and weather
	// each hit before ApplyServicesHolderDefaults existed.
	if opt := cfg.GetOptionalServices(); opt.GetBlobstore() && cfg.GetTopology().GetBlobstoreNodes() > 0 {
		if err := EnsureBlobstoreTokenKey(cfg); err != nil {
			return nil, err
		}
		if kernelConfig.Configs == nil {
			kernelConfig.Configs = &protoServicesConfig.Services{}
		}
		if kernelConfig.Configs.BlobstoreConfig == nil {
			kernelConfig.Configs.BlobstoreConfig = &protoBlobstore.Config{}
			protoBlobstore.ConfigDefaults(kernelConfig.Configs.BlobstoreConfig)
		}
		kernelConfig.Configs.BlobstoreConfig.TokenHmacKey = cfg.GetPki().GetBlobstoreTokenKey()
		// volume_groups is deliberately NOT set here. Groups are declared at build
		// time by each consuming service, through the (spec.volume_group)
		// annotation tacplus surfaces in registry.json, and rendering them is the
		// operator's job once that annotation exists — it does not yet (it is
		// tacplus's slice of the work split). Until then the list is empty, which
		// is the correct fail-closed state: no group, no upload. See
		// workers/blobstore-result.md.
	}

	// Seed the real llm_credential into kernel's services holder for every
	// service named in llm_credential_services. Kernel writes each holder entry
	// to that service's ConfigState boundary on config load, and the generated
	// chassis takes a service's typed Config only from there — the copy injected
	// into the service's own bootstrap YAML above is never read for this field.
	// The list is config-driven, so the holder entry is resolved by name
	// (<service>_config) rather than hardcoded per service.
	if cred := cfg.GetLlmCredential(); cred != "" {
		if kernelConfig.Configs == nil {
			kernelConfig.Configs = &protoServicesConfig.Services{}
		}
		holder := kernelConfig.Configs.ProtoReflect()
		for _, name := range cfg.GetLlmCredentialServices() {
			fd := holder.Descriptor().Fields().ByName(protoreflect.Name(name + "_config"))
			if fd == nil || fd.Message() == nil {
				return nil, fmt.Errorf("llm_credential_services names %q, but kernel's services holder has no %s_config entry", name, name)
			}
			svcCfg := holder.Mutable(fd).Message()
			credFd := svcCfg.Descriptor().Fields().ByName("llm_credential")
			if credFd == nil || credFd.Kind() != protoreflect.StringKind {
				return nil, fmt.Errorf("llm_credential_services names %q, but its Config declares no llm_credential string field", name)
			}
			svcCfg.Set(credFd, protoreflect.ValueOfString(cred))
		}
	}

	// Save crypto after all mutations (kernel TLS, kmaker signing CA, gateway external TLS).
	if cryptoUpdated {
		if err := SaveCrypto(cfgDir, cfg); err != nil {
			if !isReadOnlyConfigWriteError(err) {
				return nil, fmt.Errorf("saving crypto: %w", err)
			}
		}
	}

	gatewayConfigYAML, err := BuildGatewayServiceConfig(
		trustedCAs, gatewayOpts, spiffeTrustDomain, spiffeLocationPath, kernelEndpoints...,
	)
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

	// msggateway, anthropic and openai used to have their configs built here.
	// They were application services, not platform: schematic is the base an
	// application is built ON, so they moved out of this repo entirely.

	opaConfigYAML, err := buildSvc("opa", nil)
	if err != nil {
		return nil, err
	}

	// The operator's own bootstrap config — same construction as every other
	// service's, so there is exactly one way a kernel endpoint is computed.
	operatorBootstrapYAML, err := buildSvc("operator", nil)
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

	// timedb and graphdb went to the boneyard 2026-08-25 (BIG GOAL 3, first
	// move #1): out of the fleet build, protos deleted, no image published.

	var letsencryptConfigYAML []byte
	var letsencryptServiceConfigYAML []byte
	if cfg.GetTls().GetLetsencrypt() {
		letsencryptConfigYAML, err = buildSvc("letsencrypt", nil)
		if err != nil {
			return nil, err
		}
		leSvc := map[string]any{
			"fqdn":                           cfg.Fqdn,
			"hostnames":                      []string{cfg.Fqdn},
			"http_listener":                  map[string]any{"host": "0.0.0.0", "port": int64(letsencryptAcmePort)},
			"renewal_check_interval_seconds": int64(3600),
		}
		// Only set when configured; letsencrypt's own default is admin@<fqdn>.
		if email := cfg.GetTls().GetEmail(); email != "" {
			leSvc["lets_encrypt_email"] = email
		}
		letsencryptServiceConfigYAML, err = yaml.Marshal(leSvc)
		if err != nil {
			return nil, fmt.Errorf("marshaling letsencrypt service config: %w", err)
		}
	}

	var kmakerConfigYAML []byte
	if t.KmakerNodes > 0 {
		kmakerConfig, err := BuildKmakerConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("building kmaker config: %w", err)
		}
		kmakerConfigYAML, err = protoKmaker.ConfigToYAML(kmakerConfig)
		if err != nil {
			return nil, fmt.Errorf("marshaling kmaker config: %w", err)
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
		// Override disk_dir to match the Helm PVC mount path (/app/data).
		// The default "data/kernel" resolves to /data/kernel in the container
		// (working dir is /), which is not the mounted volume.
		nodeConfig.DiskDir = "/app/data/kernel"

		bsYAML, err := protoKernel.ConfigToYAML(nodeConfig)
		if err != nil {
			return nil, fmt.Errorf("marshaling bootstrap config %d: %w", i, err)
		}
		bootstrapNames = append(bootstrapNames, fmt.Sprintf("bootstrap%d", i))
		bootstrapYAMLs = append(bootstrapYAMLs, bsYAML)
	}

	dc := &DeployConfigs{
		CACertPEM:                caCertPEM,
		BootstrapNames:           bootstrapNames,
		BootstrapYAMLs:           bootstrapYAMLs,
		SentinelConfig:           sentinelConfigYAML,
		EchoConfig:               echoConfigYAML,
		MessagingConfig:          messagingConfigYAML,
		MailboxConfig:            mailboxConfigYAML,
		PersonalauthConfig:       personalauthConfigYAML,
		AppConfigs:               appConfigs,
		GatewayConfig:            gatewayConfigYAML,
		EventbusConfig:           eventbusConfigYAML,
		OauthConfig:              oauthConfigYAML,
		OpaConfig:                opaConfigYAML,
		SamlConfig:               samlConfigYAML,
		StratumConfig:            stratumConfigYAML,
		KmakerConfig:             kmakerConfigYAML,
		LetsencryptConfig:        letsencryptConfigYAML,
		LetsencryptServiceConfig: letsencryptServiceConfigYAML,
		GatewayOpts:              gatewayOpts,
		Cfg:                      cfg,
		OperatorBootstrapConfig:  operatorBootstrapYAML,
	}

	// Every artifact above is a serialized proto its consumer validates at
	// boot. Check that here, so a required field the generator stopped
	// emitting fails generation instead of a pod's boot.
	if err := ValidateDeployConfigs(dc); err != nil {
		return nil, err
	}

	return dc, nil
}

func isReadOnlyConfigWriteError(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

func hasClusterCAKey(cfg *protoOperator.Config) bool {
	return cfg != nil &&
		cfg.Pki != nil &&
		cfg.Pki.ClusterCa != nil &&
		len(cfg.Pki.ClusterCa.Chain) > 0 &&
		cfg.Pki.ClusterCa.Key != nil
}

func BuildKmakerConfig(cfg *protoOperator.Config) (*protoKmaker.Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("operator config is nil")
	}
	if cfg.Pki == nil || cfg.Pki.ClusterCa == nil || len(cfg.Pki.ClusterCa.Chain) == 0 || cfg.Pki.ClusterCa.Key == nil {
		return nil, fmt.Errorf("pki.cluster_ca with private key is required")
	}

	// Mint kmaker's own serving cert before cloning so the generated config
	// carries it; without it kmaker refuses to start (ErrTLSRequired) unless
	// the plaintext opt-in is set.
	if _, err := EnsureKmakerServingTLS(cfg); err != nil {
		return nil, err
	}

	kmakerConfig := &protoKmaker.Config{}
	if cfg.GetKmaker() != nil {
		kmakerConfig = proto.Clone(cfg.GetKmaker()).(*protoKmaker.Config)
	}

	if kmakerConfig.ClusterName == "" {
		kmakerConfig.ClusterName = cfg.Fqdn
		if kmakerConfig.ClusterName == "" {
			kmakerConfig.ClusterName = "schematic"
		}
	}
	if kmakerConfig.Listener == nil {
		kmakerConfig.Listener = &protoHost.Host{}
	}
	if kmakerConfig.Listener.Host == "" {
		kmakerConfig.Listener.Host = "0.0.0.0"
	}
	if kmakerConfig.Listener.Port == 0 {
		kmakerConfig.Listener.Port = 8443
	}
	if len(kmakerConfig.ClusterCa) == 0 {
		if cfg.Pki.RootCa != nil {
			kmakerConfig.ClusterCa = append(kmakerConfig.ClusterCa, cfg.Pki.RootCa.Chain...)
		}
		kmakerConfig.ClusterCa = append(kmakerConfig.ClusterCa, cfg.Pki.ClusterCa.Chain...)
	}
	if kmakerConfig.SigningCa == nil {
		if cfg.Pki.KmakerSigningCa == nil || len(cfg.Pki.KmakerSigningCa.Chain) == 0 || cfg.Pki.KmakerSigningCa.Key == nil {
			return nil, fmt.Errorf("pki.kmaker_signing_ca with private key is required")
		}
		kmakerConfig.SigningCa = cfg.Pki.KmakerSigningCa
	}
	if err := validateKmakerSigningCA(kmakerConfig.SigningCa, cfg.Pki.ClusterCa); err != nil {
		return nil, err
	}
	if kmakerConfig.SpiffePrefix == "" {
		kmakerConfig.SpiffePrefix = cfg.SpiffePrefix
		if kmakerConfig.SpiffePrefix == "" {
			kmakerConfig.SpiffePrefix = "local.dev"
		}
	}
	if kmakerConfig.TokenAudience == "" {
		kmakerConfig.TokenAudience = "kmaker"
	}
	if len(kmakerConfig.KernelServiceAccounts) == 0 {
		ns := cfg.Namespace
		if ns == "" {
			ns = "schematic"
		}
		kmakerConfig.KernelServiceAccounts = []*protoKmaker.KernelServiceAccount{{
			Namespace: ns,
			Name:      "kernel",
		}}
	}
	if kmakerConfig.KernelCertLifetimeSeconds <= 0 {
		kmakerConfig.KernelCertLifetimeSeconds = 86400
	}

	return kmakerConfig, nil
}

func validateKmakerSigningCA(signingCA, clusterCA *protoX509.X509CertChain) error {
	if signingCA == nil || len(signingCA.Chain) == 0 || signingCA.Key == nil {
		return fmt.Errorf("pki.kmaker_signing_ca with private key is required")
	}
	if clusterCA == nil || len(clusterCA.Chain) == 0 {
		return fmt.Errorf("pki.cluster_ca certificate is required")
	}
	if signingCA.Chain[0].Base64 == clusterCA.Chain[0].Base64 {
		return fmt.Errorf("kmaker signing_ca must not reuse the broad cluster_ca")
	}

	signingChain, err := wrapperX509.NewImmutableX509CertChain(signingCA)
	if err != nil {
		return fmt.Errorf("loading kmaker signing CA: %w", err)
	}
	clusterChain, err := wrapperX509.NewImmutableX509CertChain(clusterCA)
	if err != nil {
		return fmt.Errorf("loading cluster CA: %w", err)
	}
	signingCert := signingChain.Leaf().X509Certificate()
	if !signingCert.IsCA {
		return fmt.Errorf("kmaker signing_ca must be a CA")
	}
	if signingCert.MaxPathLen != 0 || !signingCert.MaxPathLenZero {
		return fmt.Errorf("kmaker signing_ca must have path length zero")
	}
	if err := signingCert.CheckSignatureFrom(clusterChain.Leaf().X509Certificate()); err != nil {
		return fmt.Errorf("kmaker signing_ca must be signed by cluster_ca: %w", err)
	}
	return nil
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
	check("opa", &t.OpaNodes)
	check("stratum", &t.StratumNodes)
	// The search members are the same service, so they are filtered by the same
	// name: a fleet that does not deploy stratum does not deploy half of it.
	check("stratum", &t.StratumSearchNodes)
	check("saml", &t.SamlNodes)
	check("kmaker", &t.KmakerNodes)
	check("echo", &t.EchoNodes)
	check("messaging", &t.MessagingNodes)
	check("mailbox", &t.MailboxNodes)
	check("personalauth", &t.PersonalauthNodes)
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
		{"echo", dc.EchoConfig},
		{"messaging", dc.MessagingConfig},
		{"mailbox", dc.MailboxConfig},
		{"personalauth", dc.PersonalauthConfig},
		{"gateway", dc.GatewayConfig},
		{"eventbus", dc.EventbusConfig},
		{"oauth", dc.OauthConfig},
		{"opa", dc.OpaConfig},
		{"saml", dc.SamlConfig},
		{"stratum", dc.StratumConfig},
		{"kmaker", dc.KmakerConfig},
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

	// The application's own services, keyed the same way.
	for name, data := range dc.AppConfigs {
		if len(data) > 0 {
			vals[name] = map[string]interface{}{"config": string(data)}
		}
	}

	return vals
}

// NewServiceConfigProvider returns a reconciler.ServiceConfigProvider that calls
// BuildDeployConfigs(cfgDir) on each invocation. If cfgDir is empty or the
// config directory is not yet fully initialized, it returns nil values (no override).
func NewServiceConfigProvider(cfgDir string) func() (*reconciler.ServiceConfigs, error) {
	var pkiCache *protoOperator.PKI
	// kmaker's serving cert is not part of cfg.Pki, so it is not persisted to
	// crypto.yaml and LoadConfig never returns it. Cache it here alongside
	// pkiCache: without this every reconcile mints a fresh cert, changing the
	// chart's config checksum and rolling kmaker's pods every interval.
	var kmakerTLSCache *protoX509.X509CertChain
	return func() (*reconciler.ServiceConfigs, error) {
		if cfgDir == "" {
			return nil, nil
		}
		dc, err := buildDeployConfigs(cfgDir, pkiCache, kmakerTLSCache)
		if err != nil {
			return nil, err
		}
		if dc.Cfg != nil && dc.Cfg.Pki != nil {
			pki, ok := proto.Clone(dc.Cfg.Pki).(*protoOperator.PKI)
			if !ok {
				return nil, fmt.Errorf("cloning pki cache")
			}
			pkiCache = pki
		}
		if dc.Cfg != nil && dc.Cfg.Kmaker != nil && dc.Cfg.Kmaker.Tls != nil {
			tls, ok := proto.Clone(dc.Cfg.Kmaker.Tls).(*protoX509.X509CertChain)
			if !ok {
				return nil, fmt.Errorf("cloning kmaker serving tls cache")
			}
			kmakerTLSCache = tls
		}
		return &reconciler.ServiceConfigs{
			Values: ServiceConfigValues(dc),
		}, nil
	}
}
