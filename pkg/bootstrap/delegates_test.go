package bootstrap

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
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
		// The service account is the delegate's own name for every service the
		// operator deploys; the operator itself runs as "schematic-operator",
		// the name its hand-written chart gives its ServiceAccount.
		wantKSA := delegateServiceAccountName(ServiceInstance{Service: d.Name, Tenant: tenantFromNamespace(d.TenantNamespace)})
		if d.KubernetesServiceAccounts[0].KubernetesServiceAccount != wantKSA {
			t.Fatalf("delegate %q KSA = %q, want %q", d.Name,
				d.KubernetesServiceAccounts[0].KubernetesServiceAccount, wantKSA)
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

// TestEnsureServiceDelegatesResyncsStaleNamespace reproduces the
// separate-namespace deploy bug: 'init cluster' runs EnsureServiceDelegates
// before operator.yaml's namespace is necessarily set to its final value, so
// platform delegates get their Kubernetes service-account matcher written
// with the "schematic" fallback. A later run (e.g. at 'generate' time), once
// the real namespace is configured, must correct the existing matcher rather
// than leaving it pinned to the stale value.
func TestEnsureServiceDelegatesResyncsStaleNamespace(t *testing.T) {
	dir := t.TempDir()
	setupKernelYAML(t, dir, nil)

	// First run: no namespace set yet, as at 'init cluster' time.
	unnamespaced := &protoOperator.Config{
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
	if err := EnsureServiceDelegates(dir, unnamespaced); err != nil {
		t.Fatal(err)
	}

	kernelPath := filepath.Join(dir, "services", "kernel.yaml")
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "kernel" {
			continue
		}
		if len(d.KubernetesServiceAccounts) == 0 || d.KubernetesServiceAccounts[0].KubernetesNamespace != "schematic" {
			t.Fatalf("delegate %q: want fallback namespace %q before namespace is configured, got %+v", d.Name, "schematic", d.KubernetesServiceAccounts)
		}
	}

	// Second run: namespace now configured, as at 'generate' time.
	namespaced := &protoOperator.Config{
		Namespace: "docmanager",
		Topology:  unnamespaced.Topology,
	}
	if err := EnsureServiceDelegates(dir, namespaced); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "kernel" {
			continue
		}
		if len(d.KubernetesServiceAccounts) == 0 || d.KubernetesServiceAccounts[0].KubernetesNamespace != "docmanager" {
			t.Fatalf("delegate %q: want resynced namespace %q, got %+v", d.Name, "docmanager", d.KubernetesServiceAccounts)
		}
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
			KernelNodes:   1,
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OpaNodes:      1,
			StratumNodes:  1,
			OauthNodes:    1,
			EchoNodes:     1,
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
// that an existing delegate missing its own authorized_service gets it added —
// and, since authorized_services is reconciled to the registry rather than
// appended to, that an entry the registry does not justify is dropped in the
// same pass. 'oauth' below is that entry: nothing in sentinel's dependency
// graph names it.
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
			if has["oauth"] {
				t.Fatalf("sentinel delegate kept unjustified authorized_service 'oauth': %v", d.AuthorizedServices)
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

// TestEnsureServiceDelegatesGivesAuthBrokersTheIdpKind verifies that a newly
// created oauth/saml delegate declares AUTH_BROKER_KIND_IDP. Kernel's
// MintUserToken refuses a delegate whose auth_broker_kinds does not contain the
// request's kind, so a generated cluster whose broker delegate lacks it cannot
// log anyone in.
func TestEnsureServiceDelegatesGivesAuthBrokersTheIdpKind(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		want := []string(nil)
		if d.Name == "oauth" || d.Name == "saml" {
			want = []string{"idp"}
		}
		if len(d.AuthBrokerKinds) != len(want) {
			t.Errorf("delegate %q auth_broker_kinds = %v, want %v",
				d.Name, d.AuthBrokerKinds, want)
			continue
		}
		for i := range want {
			if d.AuthBrokerKinds[i] != want[i] {
				t.Errorf("delegate %q auth_broker_kinds = %v, want %v",
					d.Name, d.AuthBrokerKinds, want)
				break
			}
		}
	}
}

// TestEnsureServiceDelegatesPreservesAuthBrokerKinds pins the round trip: a
// deployment that hand-added auth_broker_kinds to a delegate must still have it
// after the operator rewrites kernel.yaml. Before SourceDelegate carried the
// field, every rewrite silently dropped it and login broke on the next
// 'generate'.
func TestEnsureServiceDelegatesPreservesAuthBrokerKinds(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, []SourceDelegate{{
		Name:               "acmebroker",
		AuthorizedServices: []string{"kernel"},
		AuthBrokerKinds:    []string{"idp"},
	}})

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
		},
	}

	// Runs twice: the first call rewrites the file (adding platform delegates),
	// the second is the idempotent no-op path.
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != "acmebroker" {
			continue
		}
		if len(d.AuthBrokerKinds) != 1 || d.AuthBrokerKinds[0] != "idp" {
			t.Fatalf("acmebroker auth_broker_kinds = %v, want [idp]", d.AuthBrokerKinds)
		}
		return
	}
	t.Fatal("acmebroker delegate not found")
}

// TestEnsureServiceDelegatesGivesPersonalauthTheDeviceKind pins the personalauth
// delegate's shape: exactly auth_broker_kinds [device], and no tenant_namespace.
//
// The kind is what makes kernel accept a mint brokered through personalauth —
// MintUserToken refuses a delegate whose auth_broker_kinds does not contain the
// request's kind, so without it every device registration and device login fails
// closed. It must NOT also carry "idp": a broker gets exactly the kinds it
// verifies.
//
// The empty tenant_namespace is the /system root. personalauth mints
// /people/<pid> tokens, which are not tenant data and have no tenant to name.
func TestEnsureServiceDelegatesGivesPersonalauthTheDeviceKind(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:       1,
			GatewayNodes:      1,
			PersonalauthNodes: 1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Personalauth: true,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != "personalauth" {
			continue
		}
		if len(d.AuthBrokerKinds) != 1 || d.AuthBrokerKinds[0] != "device" {
			t.Fatalf("personalauth auth_broker_kinds = %v, want [device]", d.AuthBrokerKinds)
		}
		if d.TenantNamespace != "" {
			t.Fatalf("personalauth tenant_namespace = %q, want empty (/system)", d.TenantNamespace)
		}
		return
	}
	t.Fatal("personalauth delegate not found")
}

// TestEnsureServiceDelegatesSkipsPersonalauthWhenDisabled is the negative half:
// personalauth is disabled by default, so a config that does not ask for it must
// not get a delegate carrying the device broker capability.
func TestEnsureServiceDelegatesSkipsPersonalauthWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	kernelPath := setupKernelYAML(t, dir, nil)

	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
		},
	}

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "personalauth" {
			t.Fatalf("personalauth delegate created while optional_services.personalauth is false")
		}
	}
}

// TestEnsureServiceDelegatesRemovesUnjustifiedAuthorizedService reproduces the
// drift Jody found live: work, scotty and mailbox each carried a 'stratum'
// entry in authorized_services left over from an older registry shape, long
// after nothing in their spec justified it. The old code only ever appended
// missing entries, so a delegate kept every capability it had ever been
// granted. authorized_services must be reconciled to exactly the current
// registry-derived set on every run.
//
// opa carries the case here: the live registry resolves its deps to
// [eventbus kernel opa], with no stratum anywhere in them.
func TestEnsureServiceDelegatesRemovesUnjustifiedAuthorizedService(t *testing.T) {
	dir := t.TempDir()
	stale := SourceDelegate{
		Name:               "opa",
		AuthorizedServices: []string{"opa", "kernel", "eventbus", "stratum"},
	}
	kernelPath := setupKernelYAML(t, dir, []SourceDelegate{stale})

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

	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}
	want := sd.authorizedServicesFor("opa")
	if toSet(want)["stratum"] {
		t.Fatalf("test premise broken: registry now justifies stratum for opa (%v)", want)
	}

	var got []string
	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name == "opa" {
			got = d.AuthorizedServices
		}
	}
	if got == nil {
		t.Fatal("opa delegate not found")
	}
	if toSet(got)["stratum"] {
		t.Fatalf("opa delegate kept unjustified authorized_service 'stratum': %v", got)
	}
	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(sortedGot) != len(sortedWant) {
		t.Fatalf("opa authorized_services %v, want exactly %v", got, want)
	}
	for i := range sortedWant {
		if sortedGot[i] != sortedWant[i] {
			t.Fatalf("opa authorized_services %v, want exactly %v", got, want)
		}
	}

	// Idempotence: the reconciled file must not be rewritten a second time.
	// EnsureServiceDelegates returns only an error, so 'no change' is observed
	// as a byte-identical kernel.yaml (the same way TestEnsureServiceDelegates-
	// Idempotent checks it).
	first, err := os.ReadFile(kernelPath)
	if err != nil {
		t.Fatal(err)
	}
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

// TestEnsureServiceDelegatesReorderedListIsNotAChange pins the other half of
// the reconciliation: a delegate already carrying exactly the right set, but
// in a different order than the registry emits it, must not be reported as
// changed — otherwise every generate would rewrite kernel.yaml forever.
func TestEnsureServiceDelegatesReorderedListIsNotAChange(t *testing.T) {
	dir := t.TempDir()

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

	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}
	reversed := append([]string(nil), sd.authorizedServicesFor("opa")...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	kernelPath := setupKernelYAML(t, dir, []SourceDelegate{{
		Name:               "opa",
		AuthorizedServices: reversed,
		KubernetesServiceAccounts: []SourceKubernetesServiceAccount{{
			KubernetesNamespace:      operatorKubernetesNamespace(cfg),
			KubernetesServiceAccount: "opa",
		}},
	}})

	if err := EnsureServiceDelegates(dir, cfg); err != nil {
		t.Fatal(err)
	}

	for _, d := range loadDelegates(t, kernelPath) {
		if d.Name != "opa" {
			continue
		}
		if len(d.AuthorizedServices) != len(reversed) {
			t.Fatalf("opa authorized_services %v, want the same set as %v", d.AuthorizedServices, reversed)
		}
		for i := range reversed {
			if d.AuthorizedServices[i] != reversed[i] {
				t.Fatalf("already-correct opa authorized_services was rewritten: %v, was %v", d.AuthorizedServices, reversed)
			}
		}
		return
	}
	t.Fatal("opa delegate not found")
}
