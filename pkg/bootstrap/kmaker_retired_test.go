// Copyright (c) 2026 Jody Scott
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	stdx509 "crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"

	wrapperX509 "gitlab.com/seabeam/schematic/platform/services/operator/model/chassis/x509"
	protoKernel "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kernel"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

// kmakerRetiredFixture is a config dir as `init cluster` leaves it with the
// default ("small") preset, the one that used to ask for a kmaker replica.
func kmakerRetiredFixture(t *testing.T) (string, *protoOperator.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Topology:     &protoOperator.Topology{GatewayNodes: 1},
	}
	ApplyTopologyPreset(cfg, "small")
	if err := GenerateCrypto(cfg, int(cfg.Topology.KernelNodes), 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "services", "kernel.yaml"), "{}\n")
	return dir, cfg
}

func assertNoKmaker(t *testing.T, what, content string) {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(strings.ToLower(line), "kmaker") {
			hits = append(hits, line)
		}
	}
	if len(hits) > 0 {
		t.Errorf("%s mentions kmaker:\n%s", what, strings.Join(hits, "\n"))
	}
}

// TestGenerateRendersNoKmaker: kmaker is retired (#16). Nothing calls
// Kmaker.MintKernelCert (tacplus-dev/kernel#29), so `generate` must emit none
// of it: no kmaker workload, ServiceAccount or RBAC, no kmaker config, no
// kmaker-audience token on kernel, and no pki.kmaker_signing_ca, the extra CA
// private key under the cluster CA. This walks everything generate writes
// (the config dir, the bundle, the chart files and what helm renders from them).
func TestGenerateRendersNoKmaker(t *testing.T) {
	dir, _ := kmakerRetiredFixture(t)
	generateOnce(t, dir)

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		assertNoKmaker(t, "file name "+rel, rel)
		assertNoKmaker(t, rel, string(readBytes(t, path)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	b, err := BuildBundle(dir)
	if err != nil {
		t.Fatalf("BuildBundle: %v", err)
	}
	assertNoKmaker(t, "bundle config", b.Config)
	assertNoKmaker(t, "bundle crypto", b.Crypto)
	for name, svc := range b.Services {
		assertNoKmaker(t, "bundle service name", name)
		assertNoKmaker(t, "bundle service "+name, svc)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	charts, err := helm.New(CurrentSeries(), CurrentMajorMinor(), nil, nil, nil).Generate(cfg, 0, false)
	if err != nil {
		t.Fatalf("generating charts: %v", err)
	}
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		assertNoKmaker(t, "chart file name", name)
		assertNoKmaker(t, "chart file "+name, string(data))
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, ServiceConfigValues(dc))
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}
	assertNoKmaker(t, "rendered manifests", rel.Manifest)
	if !strings.Contains(rel.Manifest, "kind: StatefulSet") || !strings.Contains(rel.Manifest, "name: kernel") {
		t.Fatal("rendered manifests carry no kernel StatefulSet: the scan above is vacuous")
	}
}

// TestKernelServesWithoutKmaker: kernel never asked kmaker for its serving
// certificate; the operator signs node_tls at generate time. With kmaker gone,
// every kernel bootstrap config must still satisfy kernel's own Config
// contract and carry a serving cert and key that verify, for the name the
// gateway dials, to the root through the cluster CA.
func TestKernelServesWithoutKmaker(t *testing.T) {
	dir, cfg := kmakerRetiredFixture(t)
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatalf("generating deploy configs: %v", err)
	}
	if got, want := len(dc.BootstrapYAMLs), int(cfg.Topology.KernelNodes); got != want {
		t.Fatalf("kernel bootstrap configs = %d, want %d", got, want)
	}

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

	for i, y := range dc.BootstrapYAMLs {
		kc, err := protoKernel.ConfigFromBytes(y)
		if err != nil {
			t.Fatalf("parsing kernel config %d: %v", i, err)
		}
		protoKernel.ConfigDefaults(kc)
		if err := protoKernel.ValidateConfig(kc); err != nil {
			t.Fatalf("kernel config %d is rejected by kernel's own contract: %v", i, err)
		}
		if kc.GetTls() == nil || len(kc.GetTls().GetChain()) == 0 || kc.GetTls().GetKey() == nil {
			t.Fatalf("kernel config %d carries no serving cert and key", i)
		}
		cc, err := wrapperX509.NewImmutableX509CertChain(kc.GetTls())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cc.Leaf().X509Certificate().Verify(stdx509.VerifyOptions{
			DNSName:       "kernel",
			Roots:         roots,
			Intermediates: intermediates,
			CurrentTime:   time.Now(),
			KeyUsages:     []stdx509.ExtKeyUsage{stdx509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Fatalf("kernel %d serving cert does not verify to the root: %v", i, err)
		}
	}
}

// TestGenerateScrubsRetiredKmakerKeys: a config dir generated before kmaker was
// retired holds two copies of the kmaker signing CA private key, in
// crypto.yaml (pki.kmaker_signing_ca) and in configs/kmaker.yaml. The field
// is reserved now, so loading ignores it, but nothing would ever rewrite the
// file: the key would sit on disk for good. The next generate removes both,
// and re-mints nothing else.
func TestGenerateScrubsRetiredKmakerKeys(t *testing.T) {
	dir, _ := kmakerRetiredFixture(t)
	cryptoPath := filepath.Join(dir, OperatorCryptoFile)

	var crypto map[string]any
	if err := yaml.Unmarshal(readBytes(t, cryptoPath), &crypto); err != nil {
		t.Fatal(err)
	}
	pki, ok := crypto["pki"].(map[string]any)
	if !ok {
		t.Fatalf("crypto.yaml has no pki map: %v", crypto)
	}
	// Any CA chain with a key stands in for the old one: what matters is the
	// key under the retired name.
	pki["kmaker_signing_ca"] = pki["cluster_ca"]
	legacy, err := yaml.Marshal(crypto)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cryptoPath, string(legacy))
	writeTestFile(t, filepath.Join(dir, "configs", "kmaker.yaml"), "token_audience: kmaker\n")

	before, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("a crypto.yaml from before the retirement must still load: %v", err)
	}

	generateOnce(t, dir)

	assertNoKmaker(t, "crypto.yaml", string(readBytes(t, cryptoPath)))
	if _, err := os.Stat(filepath.Join(dir, "configs", "kmaker.yaml")); !os.IsNotExist(err) {
		t.Errorf("configs/kmaker.yaml survived generate (stat err = %v)", err)
	}
	after, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := after.GetPki().GetClusterCa().GetKey().GetId(), before.GetPki().GetClusterCa().GetKey().GetId(); got != want {
		t.Errorf("cluster CA key id = %q after the scrub, want %q unchanged", got, want)
	}
	for i := range before.GetPki().GetNodeTls() {
		if got, want := after.GetPki().GetNodeTls()[i].GetKey().GetId(), before.GetPki().GetNodeTls()[i].GetKey().GetId(); got != want {
			t.Errorf("node_tls[%d] key id = %q after the scrub, want %q unchanged", i, got, want)
		}
	}
}
