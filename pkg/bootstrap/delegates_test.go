package bootstrap

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

// setupKernelYAML creates a minimal kernel.yaml with TLS stubs and optional
// pre-existing delegates.
func setupKernelYAML(t *testing.T, dir string, delegates []SourceDelegate) string {
	t.Helper()
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &KernelSourceConfig{
		KeyRotationIntervalSeconds: 86400,
		DefaultTokenExpirySeconds:  3600,
		TLS: &TLSSourceConfig{
			Cert: "dGVzdA==",
			Key:  "dGVzdA==",
		},
		Delegates: delegates,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(servicesDir, "kernel.yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadDelegates(t *testing.T, kernelPath string) []SourceDelegate {
	t.Helper()
	cfg, err := LoadKernelSourceConfig(kernelPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Delegates
}

// TestEnsureServiceDelegatesCreatesPerService verifies that a delegate is
// created for each deployed kernel-authenticated service.
func TestEnsureServiceDelegatesCreatesPerService(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			StratumNodes:  1,
			OauthNodes:    1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	delegates := loadDelegates(t, kernelPath)
	expected := DeployedKernelAuthServices(cfg)

	if len(delegates) != len(expected) {
		t.Fatalf("got %d delegates, want %d", len(delegates), len(expected))
	}

	names := make([]string, len(delegates))
	for i, d := range delegates {
		names[i] = d.Name
	}
	sort.Strings(names)

	for i, name := range expected {
		if names[i] != name {
			t.Fatalf("delegate[%d] = %q, want %q", i, names[i], name)
		}
	}

	// Each delegate authorizes itself AND kernel: phase-2 token exchange
	// requires a token for kernel (state/mint/config), proven at runtime —
	// without it services fail with "delegate ... not authorized for service
	// kernel". Still fail-closed (no SERVICE_NAME_ANY).
	for _, d := range delegates {
		hasSelf, hasKernel := false, false
		for _, as := range d.AuthorizedServices {
			if as == d.Name {
				hasSelf = true
			}
			if as == "kernel" {
				hasKernel = true
			}
			if as == "ANY" || as == "SERVICE_NAME_ANY" {
				t.Fatalf("delegate %q must not be authorized for ANY: %v", d.Name, d.AuthorizedServices)
			}
		}
		if !hasSelf {
			t.Fatalf("delegate %q authorized_services %v missing itself", d.Name, d.AuthorizedServices)
		}
		if d.Name != "kernel" && !hasKernel {
			t.Fatalf("delegate %q authorized_services %v missing kernel", d.Name, d.AuthorizedServices)
		}
	}

	// Every delegate binds a Kubernetes service account (the field kernel
	// actually authorizes phase-1 on).
	for _, d := range delegates {
		if len(d.KubernetesServiceAccounts) == 0 {
			t.Fatalf("delegate %q has no kubernetes_service_accounts matcher", d.Name)
		}
		if d.KubernetesServiceAccounts[0].KubernetesServiceAccount != d.Name {
			t.Fatalf("delegate %q KSA = %q, want %q", d.Name,
				d.KubernetesServiceAccounts[0].KubernetesServiceAccount, d.Name)
		}
	}
}

// TestEnsureServiceDelegatesIdempotent verifies that running twice produces
// the same result.
func TestEnsureServiceDelegatesIdempotent(t *testing.T) {
	dir := t.TempDir()
	setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			StratumNodes:  1,
			OauthNodes:    1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	kernelPath := filepath.Join(dir, "services", "kernel.yaml")
	first, err := os.ReadFile(kernelPath)
	if err != nil {
		t.Fatal(err)
	}

	// Run again — must be byte-identical.
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(kernelPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(first) != string(second) {
		t.Fatalf("second run changed kernel.yaml:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestEnsureServiceDelegatesPreservesUserDelegates verifies that pre-existing
// user-added delegates (e.g. from add-api-key) survive.
func TestEnsureServiceDelegatesPreservesUserDelegates(t *testing.T) {
	dir := t.TempDir()
	userDelegate := SourceDelegate{
		Name:               "my-api-key",
		AuthorizedServices: []string{"oauth", "sentinel"},
		CustomClaims:       map[string]string{"email": "user@example.com"},
	}
	setupKernelYAML(t, dir, []SourceDelegate{userDelegate})

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			StratumNodes:  1,
			OauthNodes:    1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	kernelPath := filepath.Join(dir, "services", "kernel.yaml")
	delegates := loadDelegates(t, kernelPath)

	// User delegate must still be present.
	found := false
	for _, d := range delegates {
		if d.Name == "my-api-key" {
			found = true
			if len(d.AuthorizedServices) != 2 {
				t.Fatalf("user delegate authorized_services changed: %v", d.AuthorizedServices)
			}
			if d.CustomClaims["email"] != "user@example.com" {
				t.Fatalf("user delegate custom_claims changed: %v", d.CustomClaims)
			}
			break
		}
	}
	if !found {
		t.Fatal("user-added delegate 'my-api-key' was removed")
	}

	// Service delegates must also be present.
	expected := DeployedKernelAuthServices(cfg)
	delegateNames := make(map[string]bool, len(delegates))
	for _, d := range delegates {
		delegateNames[d.Name] = true
	}
	for _, svc := range expected {
		if !delegateNames[svc] {
			t.Fatalf("missing delegate for deployed service %q", svc)
		}
	}
}

// TestEnsureServiceDelegatesOptionalServices verifies optional services get
// delegates only when enabled.
func TestEnsureServiceDelegatesOptionalServices(t *testing.T) {
	dir := t.TempDir()
	setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:      1,
			SentinelNodes:    1,
			EventbusNodes:    1,
			GatewayNodes:     1,
			OpaNodes:         1,
			StratumNodes:     1,
			OauthNodes:       1,
			EchoNodes:        1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Saml: true,
		},
		Tls: &protoOperator.TLSConfig{
			Letsencrypt: true,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	kernelPath := filepath.Join(dir, "services", "kernel.yaml")
	delegates := loadDelegates(t, kernelPath)
	delegateNames := make(map[string]bool, len(delegates))
	for _, d := range delegates {
		delegateNames[d.Name] = true
	}

	// Enabled optional services must have delegates.
	for _, svc := range []string{"saml", "letsencrypt"} {
		if !delegateNames[svc] {
			t.Fatalf("missing delegate for enabled optional service %q", svc)
		}
	}

	// Disabled optional services must NOT have delegates. echo is the surviving
	// optional service and is left disabled above, so it carries this case.
	for _, svc := range []string{"echo"} {
		if delegateNames[svc] {
			t.Fatalf("unexpected delegate for disabled optional service %q", svc)
		}
	}
}

// TestEnsureServiceDelegatesExistingDelegateGetsAuthorizedService verifies
// that an existing delegate missing its own authorized_service gets it added.
func TestEnsureServiceDelegatesExistingDelegateGetsAuthorizedService(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing sentinel delegate authorizing oauth but not itself.
	existing := SourceDelegate{
		Name:               "sentinel",
		AuthorizedServices: []string{"oauth"},
	}
	setupKernelYAML(t, dir, []SourceDelegate{existing})

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			StratumNodes:  1,
			OauthNodes:    1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	kernelPath := filepath.Join(dir, "services", "kernel.yaml")
	delegates := loadDelegates(t, kernelPath)
	for _, d := range delegates {
		if d.Name == "sentinel" {
			// Must have both oauth (pre-existing) and sentinel (auto-added).
			has := map[string]bool{}
			for _, s := range d.AuthorizedServices {
				has[s] = true
			}
			if !has["oauth"] {
				t.Fatal("sentinel delegate lost pre-existing authorized_service 'oauth'")
			}
			if !has["sentinel"] {
				t.Fatal("sentinel delegate missing auto-added authorized_service 'sentinel'")
			}
			return
		}
	}
	t.Fatal("sentinel delegate not found")
}

// TestEnsureServiceDelegatesAppServiceGetsBroker verifies that a service
// declared under Topology.app_services gets the full registry-derived
// authorized_services, not the [self, kernel] fallback. app services are not in
// the compile-time platform list, so the registry walk skips them unless
// EnsureServiceDelegates registers them first — and a tasks delegate without
// eventbus makes every event publish PermissionDenied.
func TestEnsureServiceDelegatesAppServiceGetsBroker(t *testing.T) {
	t.Cleanup(func() { RegisterAppServices(nil) })

	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"tasks": 1},
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != "tasks" {
			continue
		}
		for _, as := range d.AuthorizedServices {
			if as == "eventbus" {
				return
			}
		}
		t.Fatalf("tasks delegate authorized_services %v missing eventbus", d.AuthorizedServices)
	}
	t.Fatal("tasks delegate not found")
}
