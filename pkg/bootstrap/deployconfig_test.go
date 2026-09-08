package bootstrap

import (
	stdx509 "crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	wrapperKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/key"
	wrapperX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/chassis/x509"
	protoGateway "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/gateway"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/host"
)

func TestApplyServiceDefaultsSetsRequiredGrpcFields(t *testing.T) {
	fields := map[string]any{}

	applyServiceDefaults(fields)

	if got := fields["max_request_size"]; got != int64(10*1024*1024) {
		t.Fatalf("max_request_size = %v, want 10485760", got)
	}
	if got := fields["request_timeout_seconds"]; got != int64(30) {
		t.Fatalf("request_timeout_seconds = %v, want 30", got)
	}
	if got := fields["shutdown_timeout_seconds"]; got != int64(30) {
		t.Fatalf("shutdown_timeout_seconds = %v, want 30", got)
	}
	listen, ok := fields["listen_address"].(map[string]any)
	if !ok {
		t.Fatalf("listen_address = %T, want map[string]any", fields["listen_address"])
	}
	if listen["host"] != "0.0.0.0" || listen["port"] != int64(8443) {
		t.Fatalf("listen_address = %#v, want 0.0.0.0:8443", listen)
	}
	if fields["logger"] == nil {
		t.Fatal("logger default was not set")
	}
}

func TestBuildBackendFieldsEmitsCanonicalSpiffeConfig(t *testing.T) {
	certChain, err := GenerateTLSCertChain()
	if err != nil {
		t.Fatal(err)
	}

	fields, err := BuildBackendFields(
		certChain.Chain,
		[]*protoHost.Host{{Host: "kernel-0.kernel", Port: 5000}},
		"local.dev",
		"aws/us-east-1/cluster-a",
	)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := fields["spiffe_prefix"]; ok {
		t.Fatalf("spiffe_prefix should not be emitted: %#v", fields["spiffe_prefix"])
	}
	if _, ok := fields["private_key"]; ok {
		t.Fatalf("private_key should not be emitted: %#v", fields["private_key"])
	}
	if got := fields["spiffe_trust_domain"]; got != "local.dev" {
		t.Fatalf("spiffe_trust_domain = %v, want local.dev", got)
	}
	if got := fields["location_path"]; got != "aws/us-east-1/cluster-a" {
		t.Fatalf("location_path = %v, want aws/us-east-1/cluster-a", got)
	}
}

func TestBuildKernelConfigAddsKubernetesServiceAccountMatchers(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, &protoOperator.Config{Namespace: "workloads"}); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	kernelYAML := []byte(`delegates:
- name: sentinel
  authorized_services: [sentinel]
- name: external-client
  authorized_services: [gateway]
`)
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), kernelYAML, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := BuildKernelConfigProto(dir)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := findDelegate(t, cfg.Delegates, "sentinel")
	if len(sentinel.KubernetesServiceAccounts) != 1 {
		t.Fatalf("sentinel KubernetesServiceAccounts = %d, want 1", len(sentinel.KubernetesServiceAccounts))
	}
	if got := sentinel.KubernetesServiceAccounts[0].GetKubernetesNamespace(); got != "workloads" {
		t.Fatalf("sentinel Kubernetes namespace = %q, want workloads", got)
	}
	if got := sentinel.KubernetesServiceAccounts[0].GetKubernetesServiceAccount(); got != "sentinel" {
		t.Fatalf("sentinel Kubernetes service account = %q, want sentinel", got)
	}
	external := findDelegate(t, cfg.Delegates, "external-client")
	if len(external.KubernetesServiceAccounts) != 0 {
		t.Fatalf("external-client should not get a default Kubernetes matcher: %#v", external.KubernetesServiceAccounts)
	}
}

func TestBuildKernelConfigCarriesDelegationGrants(t *testing.T) {
	dir := t.TempDir()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	kernelYAML := []byte(`delegates:
- name: tasks
  authorized_services: [tasks, kernel, eventbus]
grants:
- id: delegation-proof-positive
  delegate_name: tasks
  subject: spiffe://local.dev/tenants/drpepper/prod/users/joeuser
  authorized_scope:
    grants: [TASKS_ANY]
    principal_scope: PRINCIPAL_SCOPE_SELF_ONLY
  claims:
    origin_iss: https://keycloak.example/realms/drpepper
    groups: [prod]
  created_at_ms: 123
  expires_at_ms: 456
`)
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), kernelYAML, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := BuildKernelConfigProto(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Grants) != 1 {
		t.Fatalf("Grants = %d, want 1", len(cfg.Grants))
	}
	grant := cfg.Grants[0]
	if grant.GetId() != "delegation-proof-positive" {
		t.Fatalf("grant id = %q", grant.GetId())
	}
	if grant.GetDelegateName() != "tasks" {
		t.Fatalf("grant delegate = %q", grant.GetDelegateName())
	}
	if grant.GetSubject() != "spiffe://local.dev/tenants/drpepper/prod/users/joeuser" {
		t.Fatalf("grant subject = %q", grant.GetSubject())
	}
	if got := grant.GetAuthorizedScope().GetGrants(); len(got) != 1 || got[0] != "TASKS_ANY" {
		t.Fatalf("grant authorized_scope.grants = %#v, want [TASKS_ANY]", got)
	}
	if got := grant.GetAuthorizedScope().GetPrincipalScope(); got != "PRINCIPAL_SCOPE_SELF_ONLY" {
		t.Fatalf("grant principal_scope = %q", got)
	}
	if got := grant.GetClaims().GetFields()["origin_iss"].GetStringValue(); got != "https://keycloak.example/realms/drpepper" {
		t.Fatalf("grant claims.origin_iss = %q", got)
	}
	if got := grant.GetClaims().GetFields()["groups"].GetListValue().GetValues()[0].GetStringValue(); got != "prod" {
		t.Fatalf("grant claims.groups[0] = %q", got)
	}
	if grant.GetCreatedAtMs() != 123 || grant.GetExpiresAtMs() != 456 {
		t.Fatalf("grant timestamps = %d/%d, want 123/456", grant.GetCreatedAtMs(), grant.GetExpiresAtMs())
	}
}

func TestKernelServiceSpiffeURIIsCanonical(t *testing.T) {
	got, err := KernelServiceSpiffeURI("local.dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "spiffe://local.dev/system/services/kernel" {
		t.Fatalf("KernelServiceSpiffeURI = %q, want canonical kernel service URI", got)
	}
}

func TestKernelTLSNeedsRegenerationForLegacySpiffeSAN(t *testing.T) {
	cfg := &protoOperator.Config{Pki: &protoOperator.PKI{}}
	caKey, caCert, err := GenerateRootCA(cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacyChain, err := generateSignedCertChain(
		caKey,
		caCert,
		[]string{"kernel-0", "kernel0", "kernel-0.kernel", "kernel"},
		"spiffe://local/dev/kernel",
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pki.NodeTls = append(cfg.Pki.NodeTls, legacyChain)

	canonical, err := KernelServiceSpiffeURI("local.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !KernelTLSNeedsRegeneration(cfg, 1, 0, canonical) {
		t.Fatal("legacy kernel SPIFFE SAN should require TLS regeneration")
	}
}

func TestKernelTLSNeedsRegenerationAcceptsCanonicalSpiffeSAN(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	canonical, err := KernelServiceSpiffeURI("local.dev")
	if err != nil {
		t.Fatal(err)
	}
	if KernelTLSNeedsRegeneration(cfg, 1, 0, canonical) {
		t.Fatal("canonical kernel SPIFFE SAN should not require TLS regeneration")
	}
}

func TestGenerateCryptoPreservesExistingClusterCA(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	clusterCertID := cfg.Pki.ClusterCa.Chain[0].Id
	clusterKeyID := cfg.Pki.ClusterCa.Key.Id

	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}

	if got := cfg.Pki.ClusterCa.Chain[0].Id; got != clusterCertID {
		t.Fatalf("cluster CA cert id changed from %q to %q", clusterCertID, got)
	}
	if got := cfg.Pki.ClusterCa.Key.Id; got != clusterKeyID {
		t.Fatalf("cluster CA key id changed from %q to %q", clusterKeyID, got)
	}
}

func TestGenerateCryptoCreatesConstrainedKmakerSigningCA(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	if cfg.Pki.KmakerSigningCa == nil || len(cfg.Pki.KmakerSigningCa.Chain) == 0 || cfg.Pki.KmakerSigningCa.Key == nil {
		t.Fatal("kmaker signing CA was not generated")
	}
	if got, cluster := cfg.Pki.KmakerSigningCa.Chain[0].Id, cfg.Pki.ClusterCa.Chain[0].Id; got == cluster {
		t.Fatalf("kmaker signing CA reused broad cluster CA id %q", got)
	}

	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.KmakerSigningCa)
	if err != nil {
		t.Fatal(err)
	}
	cert := cc.Leaf().X509Certificate()
	if !cert.IsCA {
		t.Fatal("kmaker signing certificate must be a CA")
	}
	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Fatalf("kmaker signing CA path length = %d zero=%v, want constrained zero", cert.MaxPathLen, cert.MaxPathLenZero)
	}

	kmakerConfig, err := BuildKmakerConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kmakerConfig.SigningCa.Chain[0].Id, cfg.Pki.KmakerSigningCa.Chain[0].Id; got != want {
		t.Fatalf("kmaker config signing CA id = %q, want dedicated kmaker signing CA %q", got, want)
	}
	if got, broad := kmakerConfig.SigningCa.Chain[0].Id, cfg.Pki.ClusterCa.Chain[0].Id; got == broad {
		t.Fatalf("kmaker config signing CA reused broad cluster CA id %q", got)
	}
}

func TestKmakerSigningCAChainsToRoot(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}

	signingKey, err := wrapperKey.NewImmutableKey(cfg.Pki.KmakerSigningCa.Key)
	if err != nil {
		t.Fatal(err)
	}
	signingChain, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.KmakerSigningCa)
	if err != nil {
		t.Fatal(err)
	}
	leafChain, err := generateSignedCertChain(
		signingKey,
		signingChain.Leaf().X509Certificate(),
		[]string{"kernel-0"},
		"spiffe://local/dev/kernel/kernel-0",
	)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := wrapperX509.NewImmutableX509CertChain(leafChain)
	if err != nil {
		t.Fatal(err)
	}
	root, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.RootCa)
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		t.Fatal(err)
	}

	roots := stdx509.NewCertPool()
	roots.AddCert(root.Leaf().X509Certificate())
	intermediates := stdx509.NewCertPool()
	intermediates.AddCert(signingChain.Leaf().X509Certificate())
	intermediates.AddCert(cluster.Leaf().X509Certificate())
	if _, err := leaf.Leaf().X509Certificate().Verify(stdx509.VerifyOptions{
		DNSName:       "kernel-0",
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   time.Now(),
		KeyUsages:     []stdx509.ExtKeyUsage{stdx509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("kmaker leaf did not verify to root through kmaker signing CA: %v", err)
	}
}

func TestBuildKmakerConfigRejectsBroadClusterSigningCAOverride(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	cfg.Kmaker = &protoKmaker.Config{SigningCa: cfg.Pki.ClusterCa}

	if _, err := BuildKmakerConfig(cfg); err == nil {
		t.Fatal("expected broad cluster CA override to be rejected")
	}
}

func TestBuildBundleIncludesKmakerServiceConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.secondstarcavalier.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, serviceYAML := range bundle.Services {
		if strings.Contains(serviceYAML, "private_key:") {
			t.Fatalf("bundle service %s contains private_key:\n%s", name, serviceYAML)
		}
	}
	kmakerYAML := bundle.Services["kmaker"]
	if kmakerYAML == "" {
		t.Fatalf("bundle services missing kmaker: %#v", bundle.Services)
	}
	kmakerConfig, err := protoKmaker.ConfigFromBytes([]byte(kmakerYAML))
	if err != nil {
		t.Fatalf("parsing bundled kmaker config: %v", err)
	}
	if got := kmakerConfig.GetTokenAudience(); got != "kmaker" {
		t.Fatalf("bundled kmaker token audience = %q, want kmaker", got)
	}
	if kmakerConfig.GetSigningCa() == nil {
		t.Fatal("bundled kmaker config missing signing CA")
	}
	values := BundleServiceValues(bundle)
	if entry, ok := values["kmaker"].(map[string]interface{}); !ok || entry["config"] == "" {
		t.Fatalf("bundle service values missing kmaker config: %#v", values["kmaker"])
	}
}

// TestBundleServiceValuesCarriesNonEmptyKernelAndServiceConfigs verifies that
// BundleServiceValues translates bootstrap0..N keys to kernel0..N and that all
// enabled services carry non-empty config values — the bug that caused pods to
// start with empty configs in bundle-apply mode.
func TestBundleServiceValuesCarriesNonEmptyKernelAndServiceConfigs(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.secondstarcavalier.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   2,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			OauthNodes:    1,
			OpaNodes:      1,
			KmakerNodes:   1,
		},
	}
	if err := GenerateCrypto(cfg, 2, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	bundle, err := BuildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify raw bundle has bootstrap0/bootstrap1 keys.
	if _, ok := bundle.Services["bootstrap0"]; !ok {
		t.Fatalf("bundle.Services missing bootstrap0: keys = %v", bundleKeys(bundle))
	}
	if _, ok := bundle.Services["bootstrap1"]; !ok {
		t.Fatalf("bundle.Services missing bootstrap1: keys = %v", bundleKeys(bundle))
	}

	vals := BundleServiceValues(bundle)

	// Kernel per-replica configs must be keyed as kernelN, not bootstrapN.
	for i := range 2 {
		key := fmt.Sprintf("kernel%d", i)
		entry, ok := vals[key].(map[string]interface{})
		if !ok {
			t.Fatalf("BundleServiceValues missing %s: keys = %v", key, valKeys(vals))
		}
		cfg, _ := entry["config"].(string)
		if cfg == "" {
			t.Fatalf("%s.config is empty", key)
		}
		if !strings.Contains(cfg, "cluster_name") {
			t.Fatalf("%s.config does not look like a kernel config:\n%s", key, cfg)
		}
	}
	// bootstrapN keys must not leak into values.
	for _, bad := range []string{"bootstrap0", "bootstrap1"} {
		if _, ok := vals[bad]; ok {
			t.Fatalf("BundleServiceValues must not contain %s — should be translated to kernelN", bad)
		}
	}

	// Per-service configs must be present and non-empty.
	for _, svc := range []string{"sentinel", "gateway", "eventbus", "oauth", "opa", "kmaker"} {
		entry, ok := vals[svc].(map[string]interface{})
		if !ok {
			t.Fatalf("BundleServiceValues missing %s: keys = %v", svc, valKeys(vals))
		}
		cfg, _ := entry["config"].(string)
		if cfg == "" {
			t.Fatalf("%s.config is empty", svc)
		}
	}

	// Compare with ServiceConfigValues to ensure parity.
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	scv := ServiceConfigValues(dc)
	for key := range scv {
		if _, ok := vals[key]; !ok {
			t.Errorf("ServiceConfigValues has key %q but BundleServiceValues does not", key)
		}
	}
}

func bundleKeys(b *Bundle) []string {
	keys := make([]string, 0, len(b.Services))
	for k := range b.Services {
		keys = append(keys, k)
	}
	return keys
}

func valKeys(vals map[string]interface{}) []string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	return keys
}

func findDelegate(t *testing.T, delegates []*protoKernel.Delegate, name string) *protoKernel.Delegate {
	t.Helper()
	for _, delegate := range delegates {
		if delegate.GetName() == name {
			return delegate
		}
	}
	t.Fatalf("delegate %q not found", name)
	return nil
}

func TestEnsureKmakerSigningCABackfillsExistingClusterCrypto(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	cfg.Pki.KmakerSigningCa = nil

	if err := ensureKmakerSigningCA(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Pki.KmakerSigningCa == nil || len(cfg.Pki.KmakerSigningCa.Chain) == 0 || cfg.Pki.KmakerSigningCa.Key == nil {
		t.Fatal("kmaker signing CA was not backfilled")
	}
}

func TestGenerateKernelTLSPreservesClusterCAAndSigningCA(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 2, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	clusterCertID := cfg.Pki.ClusterCa.Chain[0].Id
	clusterKeyID := cfg.Pki.ClusterCa.Key.Id
	signingCertID := cfg.Pki.NodeSigningCa[0].Chain[0].Id
	signingKeyID := cfg.Pki.NodeSigningCa[0].Key.Id
	kmakerSigningCertID := cfg.Pki.KmakerSigningCa.Chain[0].Id
	kmakerSigningKeyID := cfg.Pki.KmakerSigningCa.Key.Id
	oldLeafID := cfg.Pki.NodeTls[0].Chain[0].Id

	if err := GenerateKernelTLS(cfg, 2, 0, "console.schematic.io", "local.dev"); err != nil {
		t.Fatal(err)
	}

	if got := cfg.Pki.ClusterCa.Chain[0].Id; got != clusterCertID {
		t.Fatalf("cluster CA cert id changed from %q to %q", clusterCertID, got)
	}
	if got := cfg.Pki.ClusterCa.Key.Id; got != clusterKeyID {
		t.Fatalf("cluster CA key id changed from %q to %q", clusterKeyID, got)
	}
	if got := cfg.Pki.NodeSigningCa[0].Chain[0].Id; got != signingCertID {
		t.Fatalf("node signing cert id changed from %q to %q", signingCertID, got)
	}
	if got := cfg.Pki.NodeSigningCa[0].Key.Id; got != signingKeyID {
		t.Fatalf("node signing key id changed from %q to %q", signingKeyID, got)
	}
	if got := cfg.Pki.KmakerSigningCa.Chain[0].Id; got != kmakerSigningCertID {
		t.Fatalf("kmaker signing cert id changed from %q to %q", kmakerSigningCertID, got)
	}
	if got := cfg.Pki.KmakerSigningCa.Key.Id; got != kmakerSigningKeyID {
		t.Fatalf("kmaker signing key id changed from %q to %q", kmakerSigningKeyID, got)
	}
	if got := cfg.Pki.NodeTls[0].Chain[0].Id; got == oldLeafID {
		t.Fatalf("kernel TLS leaf id did not change: %q", got)
	}
	if got := cfg.Pki.NodeTls[0].Chain[1].Base64; got != cfg.Pki.ClusterCa.Chain[0].Base64 {
		t.Fatalf("kernel TLS chain issuer cert does not match preserved cluster CA %q", clusterCertID)
	}
	canonical, err := KernelServiceSpiffeURI("local.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !certChainHasURI(cfg.Pki.NodeTls[0], canonical) {
		t.Fatalf("regenerated kernel TLS cert missing canonical SPIFFE URI %q", canonical)
	}
}

func TestReadOnlyConfigWriteErrorMatchesWrappedEROFSError(t *testing.T) {
	err := fmt.Errorf("saving crypto: %w", &os.PathError{
		Op:   "open",
		Path: "/config/crypto.yaml",
		Err:  syscall.EROFS,
	})
	if !isReadOnlyConfigWriteError(err) {
		t.Fatal("wrapped EROFS error should be treated as read-only config write")
	}
	if isReadOnlyConfigWriteError(fmt.Errorf("saving crypto: disk full")) {
		t.Fatal("unrelated save errors must not be treated as read-only config writes")
	}
}

func TestGatewayExternalTLSProvisionedAfterGenerate(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "gateway.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Verify gateway config has external_tls_mode set.
	gwCfg := string(dc.GatewayConfig)
	if !strings.Contains(gwCfg, "external_tls_mode: EXTERNAL_TLS_MODE_CERT_CHAIN") {
		t.Fatalf("gateway config missing external_tls_mode CERT_CHAIN:\n%s", gwCfg)
	}

	// Verify hostnames from FQDN.
	if !strings.Contains(gwCfg, "schematic.localtest.me") {
		t.Fatalf("gateway config missing FQDN hostname:\n%s", gwCfg)
	}

	// Verify cert and key are present.
	if !strings.Contains(gwCfg, "external_cert_pem:") {
		t.Fatalf("gateway config missing external_cert_pem:\n%s", gwCfg)
	}
	if !strings.Contains(gwCfg, "external_key_pem:") {
		t.Fatalf("gateway config missing external_key_pem:\n%s", gwCfg)
	}
	if !strings.Contains(gwCfg, "BEGIN CERTIFICATE") {
		t.Fatalf("gateway config external_cert_pem not PEM:\n%s", gwCfg)
	}
	if !strings.Contains(gwCfg, "PRIVATE KEY") {
		t.Fatalf("gateway config external_key_pem not PEM:\n%s", gwCfg)
	}

	// Verify root_ca is present.
	if !strings.Contains(gwCfg, "root_ca:") {
		t.Fatalf("gateway config missing root_ca:\n%s", gwCfg)
	}
}

func TestGatewayExternalTLSIdempotent(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "schematic.localtest.me", "local.dev"); err != nil {
		t.Fatal(err)
	}

	// First generation.
	generated, err := EnsureGatewayExternalTLS(cfg, "schematic.localtest.me")
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("first call should generate a cert")
	}
	firstCertID := cfg.Pki.EdgeTls[0].Chain[0].Id
	firstKeyID := cfg.Pki.EdgeTls[0].Key.Id

	// Second generation — should be idempotent.
	generated, err = EnsureGatewayExternalTLS(cfg, "schematic.localtest.me")
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Fatal("second call should skip (idempotent)")
	}
	if got := cfg.Pki.EdgeTls[0].Chain[0].Id; got != firstCertID {
		t.Fatalf("cert id changed from %q to %q on idempotent run", firstCertID, got)
	}
	if got := cfg.Pki.EdgeTls[0].Key.Id; got != firstKeyID {
		t.Fatalf("key id changed from %q to %q on idempotent run", firstKeyID, got)
	}
}

func TestGatewayExternalTLSCertHasFQDNAndLocalhost(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "schematic.localtest.me", "local.dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGatewayExternalTLS(cfg, "schematic.localtest.me"); err != nil {
		t.Fatal(err)
	}

	cc, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.EdgeTls[0])
	if err != nil {
		t.Fatal(err)
	}
	cert := cc.Leaf().X509Certificate()

	hasFQDN := false
	hasLocalhost := false
	for _, dns := range cert.DNSNames {
		if dns == "schematic.localtest.me" {
			hasFQDN = true
		}
		if dns == "localhost" {
			hasLocalhost = true
		}
	}
	if !hasFQDN {
		t.Fatalf("gateway external TLS cert missing FQDN SAN: %v", cert.DNSNames)
	}
	if !hasLocalhost {
		t.Fatalf("gateway external TLS cert missing localhost SAN: %v", cert.DNSNames)
	}
}

func TestGatewayExternalTLSPEMExtraction(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 1, 0, "schematic.localtest.me", "local.dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGatewayExternalTLS(cfg, "schematic.localtest.me"); err != nil {
		t.Fatal(err)
	}

	certPEM, keyPEM, err := GatewayExternalTLSPEM(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(certPEM, "BEGIN CERTIFICATE") {
		t.Fatalf("certPEM does not contain PEM certificate:\n%s", certPEM)
	}
	if !strings.Contains(keyPEM, "PRIVATE KEY") {
		t.Fatalf("keyPEM does not contain PEM private key:\n%s", keyPEM)
	}
}

func TestGatewayLetsencryptModeSkipsCertGeneration(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Tls:          &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	gwCfg := string(dc.GatewayConfig)
	if !strings.Contains(gwCfg, "EXTERNAL_TLS_MODE_LETSENCRYPT") {
		t.Fatalf("letsencrypt mode gateway should have LETSENCRYPT mode:\n%s", gwCfg)
	}
	if strings.Contains(gwCfg, "external_cert_pem:") {
		t.Fatalf("letsencrypt mode gateway should not have external_cert_pem:\n%s", gwCfg)
	}
}

func TestKernelBootstrapCarriesGatewayExternalTLS(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "schematic.localtest.me",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "gateway.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Parse the first kernel bootstrap YAML back to proto and verify the
	// gateway config inside kernel's services holder carries TLS fields.
	if len(dc.BootstrapYAMLs) == 0 {
		t.Fatal("no kernel bootstrap YAMLs generated")
	}
	kernelCfg, err := protoKernel.ConfigFromBytes(dc.BootstrapYAMLs[0])
	if err != nil {
		t.Fatalf("parsing kernel bootstrap YAML: %v", err)
	}
	if kernelCfg.Configs == nil {
		t.Fatal("kernel config has no services holder (Configs is nil)")
	}
	gwInKernel := kernelCfg.Configs.GatewayConfig
	if gwInKernel == nil {
		t.Fatal("kernel config has no GatewayConfig in services holder")
	}
	if gwInKernel.ExternalTlsMode != protoGateway.ExternalTlsMode_EXTERNAL_TLS_MODE_CERT_CHAIN {
		t.Fatalf("kernel gateway config external_tls_mode = %v, want CERT_CHAIN", gwInKernel.ExternalTlsMode)
	}
	if len(gwInKernel.ExternalHostnames) == 0 || gwInKernel.ExternalHostnames[0] != "schematic.localtest.me" {
		t.Fatalf("kernel gateway config external_hostnames = %v, want [schematic.localtest.me]", gwInKernel.ExternalHostnames)
	}
	if gwInKernel.ExternalCertPem == "" {
		t.Fatal("kernel gateway config external_cert_pem is empty")
	}
	if !strings.Contains(gwInKernel.ExternalCertPem, "BEGIN CERTIFICATE") {
		t.Fatalf("kernel gateway config external_cert_pem not PEM:\n%s", gwInKernel.ExternalCertPem)
	}
	if gwInKernel.ExternalKeyPem == "" {
		t.Fatal("kernel gateway config external_key_pem is empty")
	}
	if !strings.Contains(gwInKernel.ExternalKeyPem, "PRIVATE KEY") {
		t.Fatalf("kernel gateway config external_key_pem not PEM:\n%s", gwInKernel.ExternalKeyPem)
	}
	if len(gwInKernel.RootCa) == 0 {
		t.Fatal("kernel gateway config root_ca is empty")
	}
}

// TestBuildConfigOutputWritesAppServiceConfigsAndPrunesRetiredOnes covers the
// on-disk config path, which had drifted from the in-memory one.
//
// `apply` builds its bundle in memory (BundleFromConfigs), and that path always
// included AppConfigs — so app services worked and nothing pointed at the gap.
// `build` writes configs to disk and packs whatever it finds, and that path had
// no AppConfigs loop at all: the portable bundle carried no config for any
// application service, and a service started from it comes up with an empty
// config, which is a crash and not a degraded mode.
//
// The same function only ever wrote files, never removed them, so a service
// dropped from the topology kept being packed with its last config forever.
func TestBuildConfigOutputWritesAppServiceConfigsAndPrunesRetiredOnes(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.secondstarcavalier.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			AppServices:   map[string]int32{"echo": 1},
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}

	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// A config left behind by a service that is no longer in the topology.
	// anthropic-proxy was retired with the rest of proto-app and its config was
	// still being packed into bundles weeks later.
	configsDir := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configsDir, "anthropic.yaml"), []byte("stale: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A non-YAML file a human kept here must survive.
	if err := os.WriteFile(filepath.Join(configsDir, "bootstrap0.yaml.bak"), []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := BuildConfigOutput(dir); err != nil {
		t.Fatal(err)
	}

	echoCfg, err := os.ReadFile(filepath.Join(configsDir, "echo.yaml"))
	if err != nil {
		t.Fatalf("app service echo has no config on disk: %v", err)
	}
	if len(echoCfg) == 0 {
		t.Fatal("echo.yaml is empty — an empty config is a startup crash, not a default")
	}

	if _, err := os.Stat(filepath.Join(configsDir, "anthropic.yaml")); !os.IsNotExist(err) {
		t.Errorf("config for a retired service was not pruned (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(configsDir, "bootstrap0.yaml.bak")); err != nil {
		t.Errorf("a non-YAML file was removed: %v", err)
	}
	// Guard: pruning must not eat the live topology's own configs.
	for _, want := range []string{"ca.pem", "bootstrap0.yaml", "sentinel.yaml", "gateway.yaml", "eventbus.yaml"} {
		if _, err := os.Stat(filepath.Join(configsDir, want)); err != nil {
			t.Errorf("pruning removed a live config %s: %v", want, err)
		}
	}
}

func TestBuildKmakerConfigPopulatesServingTLS(t *testing.T) {
	cfg := &protoOperator.Config{Namespace: "cavalier", Fqdn: "schematic.localtest.me"}
	if err := GenerateCrypto(cfg, 1, 0, "schematic.localtest.me", "local.dev"); err != nil {
		t.Fatal(err)
	}

	kmakerConfig, err := BuildKmakerConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if kmakerConfig.Tls == nil || len(kmakerConfig.Tls.Chain) == 0 || kmakerConfig.Tls.Key == nil {
		t.Fatal("kmaker config has no serving TLS cert")
	}

	cc, err := wrapperX509.NewImmutableX509CertChain(kmakerConfig.Tls)
	if err != nil {
		t.Fatal(err)
	}
	cert := cc.Leaf().X509Certificate()
	want := []string{"kmaker", "kmaker0", "kmaker0.cavalier.svc", "kmaker0.cavalier.svc.cluster.local", "schematic.localtest.me"}
	for _, name := range want {
		found := false
		for _, dns := range cert.DNSNames {
			if dns == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("kmaker serving cert missing SAN %q: %v", name, cert.DNSNames)
		}
	}

	// The serving cert must chain to the cluster CA (and thus to the root).
	roots := stdx509.NewCertPool()
	root, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.RootCa)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(root.Leaf().X509Certificate())
	intermediates := stdx509.NewCertPool()
	cluster, err := wrapperX509.NewImmutableX509CertChain(cfg.Pki.ClusterCa)
	if err != nil {
		t.Fatal(err)
	}
	intermediates.AddCert(cluster.Leaf().X509Certificate())
	if _, err := cert.Verify(stdx509.VerifyOptions{
		DNSName:       "kmaker",
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   time.Now(),
		KeyUsages:     []stdx509.ExtKeyUsage{stdx509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("kmaker serving cert did not verify to root: %v", err)
	}
}

func TestEnsureKmakerServingTLSIdempotent(t *testing.T) {
	cfg := &protoOperator.Config{Namespace: "cavalier"}
	if err := GenerateCrypto(cfg, 1, 0, "schematic.localtest.me", "local.dev"); err != nil {
		t.Fatal(err)
	}

	generated, err := EnsureKmakerServingTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Fatal("first call should generate a cert")
	}
	firstCertID := cfg.Kmaker.Tls.Chain[0].Id
	firstKeyID := cfg.Kmaker.Tls.Key.Id

	generated, err = EnsureKmakerServingTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Fatal("second call should skip (idempotent)")
	}
	if got := cfg.Kmaker.Tls.Chain[0].Id; got != firstCertID {
		t.Fatalf("cert id changed from %q to %q on idempotent run", firstCertID, got)
	}
	if got := cfg.Kmaker.Tls.Key.Id; got != firstKeyID {
		t.Fatalf("key id changed from %q to %q on idempotent run", firstKeyID, got)
	}

	// BuildKmakerConfig must reuse the existing cert rather than minting a new one.
	kmakerConfig, err := BuildKmakerConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := kmakerConfig.Tls.Chain[0].Id; got != firstCertID {
		t.Fatalf("BuildKmakerConfig minted a new cert id %q, want reuse of %q", got, firstCertID)
	}
}

func TestEnsureKmakerServingTLSRequiresClusterCA(t *testing.T) {
	if _, err := EnsureKmakerServingTLS(&protoOperator.Config{}); err == nil {
		t.Fatal("expected error when pki.cluster_ca is absent")
	}
}
