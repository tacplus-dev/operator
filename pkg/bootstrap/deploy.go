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

	"github.com/tacplus-dev/operator/pkg/deployment"
	"github.com/tacplus-dev/operator/pkg/helm"
	"github.com/tacplus-dev/operator/pkg/reconciler"
	protoServicesConfig "github.com/tacplus-dev/operator/tacplus/types/generated/services_config"
	protoEventbus "github.com/tacplus-dev/operator/tacplus/types/services/eventbus"
	protoGateway "github.com/tacplus-dev/operator/tacplus/types/services/gateway"
	protoKernel "github.com/tacplus-dev/operator/tacplus/types/services/kernel"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
	protoDiskstore "github.com/tacplus-dev/operator/tacplus/types/sys/diskstore"
	protoHost "github.com/tacplus-dev/operator/tacplus/types/sys/host"
	protoX509 "github.com/tacplus-dev/operator/tacplus/types/sys/x509"
)

// DeployConfigs holds all generated per-service configurations.
type DeployConfigs struct {
	CACertPEM          []byte
	BootstrapNames     []string
	BootstrapYAMLs     [][]byte
	EchoConfig         []byte
	MessagingConfig    []byte
	MailboxConfig      []byte
	PersonalauthConfig []byte
	// AppConfigs holds bootstrap YAML for the application's own services, keyed
	// by service name. Platform services each have a named field above because
	// the platform's service set is fixed; an app's is not known until its
	// config declares it, so those cannot be fields.
	AppConfigs          map[string][]byte
	GatewayConfig       []byte
	EventbusConfig      []byte
	OauthConfig         []byte
	OpaConfig           []byte
	StratumConfig       []byte
	StratumSearchConfig []byte
	RedoConfig          []byte
	BlobstoreConfig     []byte
	GatewayOpts         GatewayOptions
	Cfg                 *protoOperator.Config
	// Kernel is the kernel Config every node's bootstrap is built from, its
	// defaults applied: what the chart must agree with kernel on (its Raft
	// port, helm.KernelRaftPort) comes from here.
	Kernel *protoKernel.Config

	// OperatorBootstrapConfig is the operator's OWN chassis grpcconfig.Config —
	// the same shape every other service gets, built from the same kernel
	// endpoints, trusted CAs and SPIFFE fields. Nothing else generates this file
	// for it: it is the thing that generates everyone else's. Carries no private
	// key (bootstrap is a projected k8s token). In cluster the operator uses
	// it to read kernel's tenant records (startKernelConfigState).
	OperatorBootstrapConfig []byte
}

// BuildDeployConfigs loads config from cfgDir, generates crypto if needed, builds all
// per-service configs, and returns them in a DeployConfigs struct.
func BuildDeployConfigs(cfgDir string) (*DeployConfigs, error) {
	return buildDeployConfigs(cfgDir, nil, nil, nil)
}

// buildDeployConfigs is BuildDeployConfigs with kernel's delegates extended by
// recordTenants, whose tids recordTids gives (see buildKernelConfigProto).
func buildDeployConfigs(cfgDir string, pkiOverride *protoOperator.PKI, recordTenants []string, recordTids map[string]string) (*DeployConfigs, error) {
	cfg, err := LoadConfig(cfgDir)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if err := deployment.Validate(cfg); err != nil {
		return nil, err
	}
	// Where the platform runs; every endpoint below names it in full.
	ns := deployment.PlatformNamespace(cfg)
	if pkiOverride != nil {
		pki, ok := proto.Clone(pkiOverride).(*protoOperator.PKI)
		if !ok {
			return nil, fmt.Errorf("cloning cached pki")
		}
		cfg.Pki = pki
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

	kernelConfig, err := buildKernelConfigProto(cfgDir, recordTenants, recordTids)
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
	// stratum's seed_members is not in its holder entry: it moved to the
	// tenant-scope StratumTenantConfig (stratum#26), which reaches stratum
	// through TenantConfig.stratum once kernel fills it from its bootstrap
	// tenants (kernel#34) and #2 renders each tenant's list.
	// redo's data_dir is where the chart mounts its volume.
	if rc := kernelConfig.Configs.RedoConfig; rc != nil {
		rc.DataDir = helm.RedoDataDir
	}
	spiffeTrustDomain := kernelConfig.GetSpiffeTrustDomain()
	// operator.Config.spiffe_prefix must equal kernel's trust domain; the
	// holder's operator entry is otherwise left at its local.dev default.
	if oc := kernelConfig.Configs.OperatorConfig; oc != nil {
		oc.SpiffePrefix = spiffeTrustDomain
	}
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
	// A crypto.yaml written before kmaker was retired (#16) still holds
	// pki.kmaker_signing_ca, a CA private key nothing uses any more. Loading
	// ignores the reserved field; only rewriting the file drops the key.
	if cryptoHoldsRetiredKeys(cfgDir) {
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
	_ = writeConfigFile(filepath.Join(cfgDir, "ca.pem"), caCertPEM)

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

	// operator.yaml's tenants with their tids, so kernel keeps as each
	// tenant's id the one its namespace was rendered with (kernel#3).
	if err := addBootstrapTenants(kernelConfig, cfg); err != nil {
		return nil, err
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

	// Full service names: a service in a tenant namespace resolves a short name
	// in its own namespace, not in the platform's (#3).
	kernelEndpoints := make([]*protoHost.Host, nodeCount)
	for i := range nodeCount {
		kernelEndpoints[i] = &protoHost.Host{Host: deployment.ServiceHost(fmt.Sprintf("kernel-%d.kernel", i), ns), Port: int32(protoKernel.DefaultListenerPort)}
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
	// Every public name, fqdn first. The OIDC issuer is built from fqdn alone
	// (Issuer) and extra_hostnames never reaches it. The gateway serves one
	// certificate and has no SNI selection, so extra_hostnames must stay empty
	// (decisions/extra-hostnames-2026-09-30.md).
	publicHostnames := append([]string{cfg.Fqdn}, cfg.GetExtraHostnames()...)
	if cfg.Fqdn != "" {
		gatewayOpts.Hostnames = publicHostnames
	}

	// Ensure gateway has a cluster-CA external TLS cert. The gateway serves
	// the chain it is given (CERT_CHAIN, its only mode) and hot-swaps a new
	// one when its config carries it: the deployment's pipeline delivers the
	// public certificate that way (tacplus-dev/roadmap
	// decisions/certificates-by-pipeline-2026-11-03.md).
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
		gwCfg.ExternalTlsMode = protoGateway.ExternalTlsMode_EXTERNAL_TLS_MODE_CERT_CHAIN
		// The edge forwards no port 80, so nothing binds a plaintext listener.
		gwCfg.HttpListener = nil
		gwCfg.ExternalHostnames = gatewayOpts.Hostnames
		gwCfg.ExternalCertPem = gatewayOpts.ExternalCertPEM
		gwCfg.ExternalKeyPem = gatewayOpts.ExternalKeyPEM
		gwCfg.RootCa = gatewayOpts.RootCAs
	}

	// Seed the real llm_credential into kernel's services holder for every
	// service named in llm_credential_services. Kernel writes each holder entry
	// to that service's ConfigState boundary on config load, and the generated
	// chassis takes a service's typed Config only from there — the copy injected
	// into the service's own bootstrap YAML above is never read for this field.
	// The list is config-driven, so the holder entry is resolved by name
	// (<service>_config) rather than hardcoded per service.
	//
	// THIS IS THE SAME EXPOSURE blobstore's token_hmac_key had, and it is still
	// here on purpose. The combined seed is one blob every config-holder in the
	// fleet can read, so this hands the fleet's one real LLM API key to every
	// service. The fix is identical to blobstore's — move the field off
	// llmproxy.Config onto an llmproxy.Secrets message and deliver it with
	// serviceSpec.SecretEnvVars — the generic per-pod Secret mechanism in
	// pkg/helm/generator.go, which is built and tested and has no declarer yet.
	// It cannot be made here: llmproxy.proto lives in the application proto repo,
	// not in platform/proto. Removing the seed WITHOUT that proto change is a
	// known outage: llmproxy reads this field from ConfigState and nowhere else,
	// and ran with an empty key answering 401 the last time it only reached the
	// bootstrap YAML (TestKernelBootstrapSeedsLlmCredentialIntoServicesHolder).
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

	// Save crypto after all mutations (kernel TLS, gateway external TLS).
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

	// disk_persistence (and with it the archive) is a bootstrap setting, so the
	// writers get it here, verbatim from operator.yaml, when it is set.
	var stratumExtra func(map[string]any)
	if dp := cfg.GetStratumDiskPersistence(); dp != nil {
		dpYAML, err := protoDiskstore.DiskPersistenceConfigToYAML(dp)
		if err != nil {
			return nil, fmt.Errorf("marshaling stratum disk_persistence: %w", err)
		}
		var dpMap map[string]any
		if err := yaml.Unmarshal(dpYAML, &dpMap); err != nil {
			return nil, fmt.Errorf("unmarshaling stratum disk_persistence YAML: %w", err)
		}
		stratumExtra = func(f map[string]any) { f["disk_persistence"] = dpMap }
	}
	stratumConfigYAML, err := buildSvc("stratum", stratumExtra)
	if err != nil {
		return nil, err
	}

	// The search members get the plain stratum bootstrap, never the writers':
	// they have no volume for disk_persistence.dir to point at.
	var stratumSearchConfigYAML []byte
	if cfg.GetTopology().GetStratumSearchNodes() > 0 {
		stratumSearchConfigYAML, err = buildSvc("stratum", nil)
		if err != nil {
			return nil, err
		}
	}

	// timedb and graphdb went to the boneyard 2026-08-25 (BIG GOAL 3, first
	// move #1): out of the fleet build, protos deleted, no image published.

	// redo takes the plain shape: its typed Config comes from the holder.
	var redoConfigYAML []byte
	if cfg.GetTopology().GetStratumNodes() > 0 {
		redoConfigYAML, err = buildSvc(redoServiceName, nil)
		if err != nil {
			return nil, err
		}
	}

	// blobstore is a StatefulSet on the generic chassis: its per-ordinal
	// config Secrets copy this one bootstrap, exactly as stratum's do.
	var blobstoreConfigYAML []byte
	if opt := cfg.GetOptionalServices(); opt.GetBlobstore() && cfg.GetTopology().GetBlobstoreNodes() > 0 {
		blobstoreConfigYAML, err = buildSvc("blobstore", nil)
		if err != nil {
			return nil, err
		}
	}

	// Per-node kernel bootstrap configs.
	members := make([]*protoHost.Host, nodeCount)
	for i := range nodeCount {
		members[i] = &protoHost.Host{Host: deployment.ServiceHost(fmt.Sprintf("kernel-%d.kernel", i), ns), Port: int32(protoKernel.DefaultHazelcastListenerPort)}
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
		CACertPEM:               caCertPEM,
		BootstrapNames:          bootstrapNames,
		BootstrapYAMLs:          bootstrapYAMLs,
		EchoConfig:              echoConfigYAML,
		MessagingConfig:         messagingConfigYAML,
		MailboxConfig:           mailboxConfigYAML,
		PersonalauthConfig:      personalauthConfigYAML,
		AppConfigs:              appConfigs,
		GatewayConfig:           gatewayConfigYAML,
		EventbusConfig:          eventbusConfigYAML,
		OauthConfig:             oauthConfigYAML,
		OpaConfig:               opaConfigYAML,
		StratumConfig:           stratumConfigYAML,
		StratumSearchConfig:     stratumSearchConfigYAML,
		RedoConfig:              redoConfigYAML,
		BlobstoreConfig:         blobstoreConfigYAML,
		GatewayOpts:             gatewayOpts,
		Cfg:                     cfg,
		Kernel:                  kernelConfig,
		OperatorBootstrapConfig: operatorBootstrapYAML,
	}

	// Every artifact above is a serialized proto its consumer validates at
	// boot. Check that here, so a required field the generator stopped
	// emitting fails generation instead of a pod's boot.
	if err := ValidateDeployConfigs(dc); err != nil {
		return nil, err
	}

	return dc, nil
}

// addBootstrapTenants lists in kernel's Config.tenants every tenant of cfg
// that tenant_ids gives a tid, other than the reserved ones (kernel has
// those). A tenant kernel.yaml already lists keeps its entry; the same name
// with another tid is refused, since a tid is immutable.
func addBootstrapTenants(kernelConfig *protoKernel.Config, cfg *protoOperator.Config) error {
	listed := make(map[string]string, len(kernelConfig.GetTenants()))
	for _, t := range kernelConfig.GetTenants() {
		listed[t.GetName()] = t.GetTid()
	}
	for _, name := range cfg.GetTenants() {
		tid, ok := cfg.GetTenantIds()[name]
		if !ok || name == deployment.PeopleTenant || name == deployment.RestoreTenant {
			continue
		}
		if have, ok := listed[name]; ok {
			if have != "" && have != tid {
				return fmt.Errorf("tenant %q: kernel.yaml lists tid %s, operator.yaml %s", name, have, tid)
			}
			continue
		}
		kernelConfig.Tenants = append(kernelConfig.Tenants, &protoKernel.BootstrapTenant{Name: name, Tid: tid})
	}
	return nil
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
	check("eventbus", &t.EventbusNodes)
	check("gateway", &t.GatewayNodes)
	check("oauth", &t.OauthNodes)
	check("opa", &t.OpaNodes)
	check("stratum", &t.StratumNodes)
	// The search members are the same service, so they are filtered by the same
	// name: a fleet that does not deploy stratum does not deploy half of it.
	check("stratum", &t.StratumSearchNodes)
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
		{"echo", dc.EchoConfig},
		{"messaging", dc.MessagingConfig},
		{"mailbox", dc.MailboxConfig},
		{"personalauth", dc.PersonalauthConfig},
		{"gateway", dc.GatewayConfig},
		{"eventbus", dc.EventbusConfig},
		{"oauth", dc.OauthConfig},
		{"opa", dc.OpaConfig},
		{"stratum", dc.StratumConfig},
		{"stratum-search", dc.StratumSearchConfig},
		{redoInstanceName, dc.RedoConfig},
		{"blobstore", dc.BlobstoreConfig},
	} {
		if len(pair.data) > 0 {
			vals[pair.name] = map[string]interface{}{"config": string(pair.data)}
		}
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
//
// records, when non-nil, returns kernel's ConfigState as of this pass. The
// tenants it records that operator.yaml does not list (recordTenants) get
// their kernel delegates in memory and come back as ServiceConfigs.Tenants for
// the chart; nothing is written to the config dir for them.
func NewServiceConfigProvider(cfgDir string, records func() *protoKernel.ConfigState) func() (*reconciler.ServiceConfigs, error) {
	var pkiCache *protoOperator.PKI
	return func() (*reconciler.ServiceConfigs, error) {
		if cfgDir == "" {
			return nil, nil
		}
		var tenants []string
		var tids map[string]string
		var complete bool
		var refused error
		if records != nil {
			cfg, err := LoadConfig(cfgDir)
			if err != nil {
				return nil, err
			}
			state := records()
			tenants, tids, refused = recordTenants(cfg, state)
			// Only a read of kernel's records may delete a tenant's namespace:
			// before the first one, the tenants they hold are simply missing.
			// Kernel always records its reserved people tenant, so a state with
			// no tenant is not a read yet.
			complete = len(state.GetTenants()) > 0
		}
		dc, err := buildDeployConfigs(cfgDir, pkiCache, tenants, tids)
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
		return &reconciler.ServiceConfigs{
			Values:          ServiceConfigValues(dc),
			Tenants:         tenants,
			TenantIds:       tids,
			TenantsComplete: complete,
			RefusedTenants:  refused,
			Kernel:          dc.Kernel,
		}, nil
	}
}
