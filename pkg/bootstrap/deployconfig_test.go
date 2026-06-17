package bootstrap

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	protoHost "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/libs/host"
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
	_, privateKey, err := GenerateCredPair()
	if err != nil {
		t.Fatal(err)
	}
	certChain, err := GenerateTLSCertChain()
	if err != nil {
		t.Fatal(err)
	}

	fields, err := BuildBackendFields(
		privateKey,
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
	if got := fields["spiffe_trust_domain"]; got != "local.dev" {
		t.Fatalf("spiffe_trust_domain = %v, want local.dev", got)
	}
	if got := fields["location_path"]; got != "aws/us-east-1/cluster-a" {
		t.Fatalf("location_path = %v, want aws/us-east-1/cluster-a", got)
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

func TestGenerateKernelTLSPreservesClusterCAAndSigningCA(t *testing.T) {
	cfg := &protoOperator.Config{}
	if err := GenerateCrypto(cfg, 2, 0, "console.cavalier.io", "local.dev"); err != nil {
		t.Fatal(err)
	}
	clusterCertID := cfg.Pki.ClusterCa.Chain[0].Id
	clusterKeyID := cfg.Pki.ClusterCa.Key.Id
	signingCertID := cfg.Pki.NodeSigningCa[0].Chain[0].Id
	signingKeyID := cfg.Pki.NodeSigningCa[0].Key.Id
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
