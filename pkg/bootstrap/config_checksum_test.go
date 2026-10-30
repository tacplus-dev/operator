package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	k8syaml "sigs.k8s.io/yaml"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

// TestSeedChangeRollsKernelPods pins the link between kernel's rendered config
// Secret and the StatefulSet annotation that rolls its pods.
//
// A seed-only change — a new delegate in services/kernel.yaml, no topology or
// image change — reaches the pods only if the checksum/config-N annotation moves
// with the Secret's content. When it does not, the new Secret is written and
// nothing restarts: kernel keeps serving the old seed until someone runs
// `rollout restart sts/kernel` by hand, which is how a delegate fix silently
// failed to apply on operator rev 24.
//
// So the assertion is not "the annotation changed" alone but "the annotation is
// the sha256 of exactly the config.yaml the Secret carries", in both revisions.
// Either half drifting on its own reproduces the failure.
func TestSeedChangeRollsKernelPods(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
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
	kernelYAML := filepath.Join(servicesDir, "kernel.yaml")
	writeSeed := func(delegates string) {
		if err := os.WriteFile(kernelYAML, []byte(delegates), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// The chart itself is generated once and never regenerated: the only thing
	// that changes between the two revisions below is the seed.
	charts, err := helm.New("test", "1.2", nil, nil, nil).Generate(cfg, 1, false)
	if err != nil {
		t.Fatalf("generating chart: %v", err)
	}
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}

	provider := NewServiceConfigProvider(dir, nil)

	// Revision one: one delegate.
	writeSeed(`delegates:
  - name: opa
    authorized_services: [opa, kernel]
`)
	first, err := provider()
	if err != nil {
		t.Fatal(err)
	}
	firstSecret, firstChecksum := kernelSecretAndChecksum(t, ch, first.Values)
	if want := sha256Hex(firstSecret); firstChecksum != want {
		t.Fatalf("checksum/config-0 %q does not match the rendered Secret content (sha256 %q)", firstChecksum, want)
	}

	// Revision two: the seed gains a delegate and NOTHING else changes.
	writeSeed(`delegates:
  - name: opa
    authorized_services: [opa, kernel]
  - name: blobstore
    authorized_services: [blobstore, kernel, stratum]
`)
	second, err := provider()
	if err != nil {
		t.Fatal(err)
	}
	secondSecret, secondChecksum := kernelSecretAndChecksum(t, ch, second.Values)

	if secondSecret == firstSecret {
		t.Fatal("guard: the new delegate did not change kernel's rendered config, so this test cannot observe the checksum")
	}
	if !strings.Contains(secondSecret, "blobstore") {
		t.Fatalf("guard: the new delegate is not in kernel's rendered config:\n%s", secondSecret)
	}
	if want := sha256Hex(secondSecret); secondChecksum != want {
		t.Fatalf("checksum/config-0 %q does not match the new Secret content (sha256 %q): a seed-only change writes the Secret and never rolls the pod", secondChecksum, want)
	}
	if secondChecksum == firstChecksum {
		t.Fatalf("the Secret content changed but checksum/config-0 stayed %q: kernel keeps the old seed until someone restarts it by hand", firstChecksum)
	}
}

// kernelSecretAndChecksum renders the chart exactly as the reconciler's upgrade
// will render it and returns kernel-0's config.yaml alongside the
// checksum/config-0 annotation the kernel StatefulSet's pod template carries.
func kernelSecretAndChecksum(t *testing.T, ch *helmchart.Chart, vals map[string]interface{}) (string, string) {
	t.Helper()

	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic"
	install.Namespace = "schematic"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, vals)
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}

	var secret, checksum string
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &head); err != nil {
			continue
		}
		switch {
		case head.Kind == "Secret" && head.Metadata.Name == "kernel-0-config":
			var sec struct {
				StringData map[string]string `json:"stringData"`
			}
			if err := k8syaml.Unmarshal([]byte(doc), &sec); err != nil {
				t.Fatalf("decoding kernel-0-config: %v", err)
			}
			secret = sec.StringData["config.yaml"]
		case head.Kind == "StatefulSet" && head.Metadata.Name == "kernel":
			var st appsv1.StatefulSet
			if err := k8syaml.Unmarshal([]byte(doc), &st); err != nil {
				t.Fatalf("decoding kernel statefulset: %v", err)
			}
			checksum = st.Spec.Template.Annotations["checksum/config-0"]
		}
	}
	if secret == "" {
		t.Fatal("chart rendered no kernel-0-config Secret content")
	}
	if checksum == "" {
		t.Fatal("kernel StatefulSet pod template carries no checksum/config-0 annotation")
	}
	return secret, checksum
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestOauthProviderChangeRollsKernelPods is the same link for a service's own
// bootstrap config rather than kernel's. Kernel seeds every service's config
// holder from services/<name>.yaml, so a provider added to oauth.yaml must
// move kernel's checksum, or kernel keeps re-seeding oauth's old holder (the
// Microsoft provider on 2026-10-11).
func TestOauthProviderChangeRollsKernelPods(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			OauthNodes:  1,
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
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("delegates: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	const google = `  - name: google
    enable: true
    issuer: https://accounts.google.com
    client_id: google-client
    allowed_redirect_uri_prefixes: [https://console.seabeam.cloud/]
`
	const microsoft = `  - name: microsoft
    enable: true
    issuer: https://login.microsoftonline.com/common/v2.0
    client_id: microsoft-client
    allowed_redirect_uri_prefixes: [https://console.seabeam.cloud/]
`
	writeOauth := func(providers string) {
		if err := os.WriteFile(filepath.Join(servicesDir, "oauth.yaml"), []byte("providers:\n"+providers), 0644); err != nil {
			t.Fatal(err)
		}
	}

	charts, err := helm.New("test", "1.2", nil, nil, nil).Generate(cfg, 1, false)
	if err != nil {
		t.Fatalf("generating chart: %v", err)
	}
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}
	provider := NewServiceConfigProvider(dir, nil)

	writeOauth(google)
	first, err := provider()
	if err != nil {
		t.Fatal(err)
	}
	_, firstChecksum := kernelSecretAndChecksum(t, ch, first.Values)

	writeOauth(google + microsoft)
	second, err := provider()
	if err != nil {
		t.Fatal(err)
	}
	secondSecret, secondChecksum := kernelSecretAndChecksum(t, ch, second.Values)

	if !strings.Contains(secondSecret, "microsoft-client") {
		t.Fatalf("guard: the new provider is not in kernel's rendered config:\n%s", secondSecret)
	}
	if want := sha256Hex(secondSecret); secondChecksum != want {
		t.Fatalf("checksum/config-0 %q does not match the new Secret content (sha256 %q)", secondChecksum, want)
	}
	if secondChecksum == firstChecksum {
		t.Fatalf("oauth.yaml gained a provider but kernel's checksum/config-0 stayed %q: kernel keeps seeding oauth's old config holder", firstChecksum)
	}
}
