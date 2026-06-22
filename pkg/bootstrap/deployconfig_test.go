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

	wrapperKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/libs/key"
	wrapperX509 "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/libs/x509"
	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/libs/host"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
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
	if len(sentinel.CredPairs) != 0 {
		t.Fatalf("sentinel CredPairs = %d, want 0", len(sentinel.CredPairs))
	}

	external := findDelegate(t, cfg.Delegates, "external-client")
	if len(external.KubernetesServiceAccounts) != 0 {
		t.Fatalf("external-client should not get a default Kubernetes matcher: %#v", external.KubernetesServiceAccounts)
	}
	if len(external.CredPairs) != 0 {
		t.Fatalf("external-client CredPairs = %d, want 0", len(external.CredPairs))
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	clusterCertID := cfg.Pki.ClusterCa.Chain[0].Id
	clusterKeyID := cfg.Pki.ClusterCa.Key.Id

	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
		Namespace:    "cavalier",
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
	if err := GenerateCrypto(cfg, 1, 0, "console.cavalier.io", "local.dev"); err != nil {
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
	if err := GenerateCrypto(cfg, 2, 0, "console.cavalier.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	clusterCertID := cfg.Pki.ClusterCa.Chain[0].Id
	clusterKeyID := cfg.Pki.ClusterCa.Key.Id
	signingCertID := cfg.Pki.NodeSigningCa[0].Chain[0].Id
	signingKeyID := cfg.Pki.NodeSigningCa[0].Key.Id
	kmakerSigningCertID := cfg.Pki.KmakerSigningCa.Chain[0].Id
	kmakerSigningKeyID := cfg.Pki.KmakerSigningCa.Key.Id
	oldLeafID := cfg.Pki.NodeTls[0].Chain[0].Id

	if err := GenerateKernelTLS(cfg, 2, 0, "console.cavalier.io", "local.dev"); err != nil {
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
