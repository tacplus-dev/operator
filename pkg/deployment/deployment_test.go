package deployment

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

func TestValidateNameAcceptsTheDecidedShape(t *testing.T) {
	for _, name := range []string{"a", "prod", "seatrials", "a1", "dev-2", strings.Repeat("a", MaxNameLength)} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateNameRefusesEverythingElse(t *testing.T) {
	for _, name := range []string{
		"",                                   // empty
		strings.Repeat("a", MaxNameLength+1), // 17 characters
		"1prod",                              // starts with a digit
		"-prod",                              // starts with a hyphen
		"prod-",                              // ends with a hyphen: tac-prod- is no namespace
		"Prod",                               // upper case
		"pr_od",                              // underscore
		"pr.od",                              // dot
	} {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

func TestValidateTid(t *testing.T) {
	for _, tid := range []string{"a1b2c3d4", "zzzzzzzz", "00000001"} {
		if err := ValidateTid(tid); err != nil {
			t.Errorf("ValidateTid(%q) = %v, want nil", tid, err)
		}
	}
	for _, tid := range []string{"", "a1b2c3d", "a1b2c3d45", "A1B2C3D4", "a1b2-3d4"} {
		if err := ValidateTid(tid); err == nil {
			t.Errorf("ValidateTid(%q) = nil, want an error", tid)
		}
	}
}

func TestNamespaces(t *testing.T) {
	if got := Namespace("prod"); got != "tac-prod" {
		t.Errorf("Namespace(prod) = %q, want tac-prod", got)
	}
	if got := TenantNamespace("prod", "a1b2c3d4"); got != "tac-prod-a1b2c3d4" {
		t.Errorf("TenantNamespace(prod, a1b2c3d4) = %q, want tac-prod-a1b2c3d4", got)
	}
	if got := TenantNamespace("prod", PeopleTid); got != "tac-prod-00000001" {
		t.Errorf("people's namespace = %q, want tac-prod-00000001", got)
	}
}

// The longest deployment name and a tid still make a namespace Kubernetes
// accepts: a DNS-1123 label of at most 63 characters.
func TestLongestNamesFitTheNamespaceLimit(t *testing.T) {
	longest := strings.Repeat("a", MaxNameLength)
	for _, ns := range []string{Namespace(longest), TenantNamespace(longest, "a1b2c3d4")} {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			t.Errorf("%q is not a namespace name: %v", ns, errs)
		}
		if len(ns) > validation.DNS1123LabelMaxLength {
			t.Errorf("%q is longer than %d", ns, validation.DNS1123LabelMaxLength)
		}
	}
}

func TestServiceHost(t *testing.T) {
	if got := ServiceHost("kernel-0.kernel", "tac-prod"); got != "kernel-0.kernel.tac-prod.svc.cluster.local" {
		t.Errorf("ServiceHost = %q", got)
	}
}

func cfgWith(dep string, tenants []string, tids map[string]string) *protoOperator.Config {
	return &protoOperator.Config{Deployment: dep, Tenants: tenants, TenantIds: tids}
}

func TestPlatformNamespace(t *testing.T) {
	for _, tc := range []struct {
		cfg  *protoOperator.Config
		want string
	}{
		{&protoOperator.Config{}, "schematic"},
		{&protoOperator.Config{Namespace: "fleet"}, "fleet"},
		{&protoOperator.Config{Deployment: "prod"}, "tac-prod"},
		{&protoOperator.Config{Deployment: "prod", Namespace: "schematic"}, "tac-prod"},
	} {
		if got := PlatformNamespace(tc.cfg); got != tc.want {
			t.Errorf("PlatformNamespace(%v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

func TestTenantNamespaceOf(t *testing.T) {
	cfg := cfgWith("prod", []string{"acme"}, map[string]string{"acme": "a1b2c3d4"})
	if got := TenantNamespaceOf(cfg, "acme"); got != "tac-prod-a1b2c3d4" {
		t.Errorf("TenantNamespaceOf(acme) = %q", got)
	}
	// One namespace before deployments: a tenant's services share it.
	legacy := &protoOperator.Config{Namespace: "schematic", Tenants: []string{"acme"}}
	if got := TenantNamespaceOf(legacy, "acme"); got != "schematic" {
		t.Errorf("legacy TenantNamespaceOf(acme) = %q, want schematic", got)
	}
}

func TestValidateConfig(t *testing.T) {
	ok := []*protoOperator.Config{
		{},                   // the one-namespace layout
		{Namespace: "fleet"}, // likewise
		cfgWith("prod", nil, nil),
		cfgWith("prod", []string{"acme", "beta"}, map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"}),
		{Deployment: "prod", Namespace: "schematic"}, // namespace's default
		{Deployment: "prod", Namespace: "tac-prod"},
		// A reserved tenant may be listed (a service pinned to it), with its
		// reserved tid or none.
		cfgWith("prod", []string{"people", "restore"}, map[string]string{"people": PeopleTid}),
	}
	for _, cfg := range ok {
		if err := Validate(cfg); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", cfg, err)
		}
	}
	bad := map[string]*protoOperator.Config{
		"bad name":               cfgWith("Prod", nil, nil),
		"other namespace":        {Deployment: "prod", Namespace: "fleet"},
		"tenant without tid":     cfgWith("prod", []string{"acme"}, nil),
		"bad tid":                cfgWith("prod", []string{"acme"}, map[string]string{"acme": "a1b2"}),
		"all-digit tid":          cfgWith("prod", []string{"acme"}, map[string]string{"acme": "12345678"}),
		"shared tid":             cfgWith("prod", []string{"acme", "beta"}, map[string]string{"acme": "a1b2c3d4", "beta": "a1b2c3d4"}),
		"tid for no tenant":      cfgWith("prod", []string{"acme"}, map[string]string{"acme": "a1b2c3d4", "gone": "b1c2d3e4"}),
		"people's tid changed":   cfgWith("prod", []string{"people"}, map[string]string{"people": "a1b2c3d4"}),
		"bad tid, no deployment": {Tenants: []string{"acme"}, TenantIds: map[string]string{"acme": "a1b2"}},
	}
	for name, cfg := range bad {
		if err := Validate(cfg); err == nil {
			t.Errorf("%s: Validate(%v) = nil, want an error", name, cfg)
		}
	}
}

func TestTenantNamespacesIncludeTheReservedTenants(t *testing.T) {
	cfg := cfgWith("prod", []string{"acme"}, map[string]string{"acme": "a1b2c3d4"})
	got := TenantTids(cfg)
	want := map[string]string{"acme": "a1b2c3d4", PeopleTenant: PeopleTid, RestoreTenant: RestoreTid}
	if len(got) != len(want) {
		t.Fatalf("TenantTids = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("TenantTids[%s] = %q, want %q", k, got[k], v)
		}
	}
	if got := TenantTids(&protoOperator.Config{Tenants: []string{"acme"}}); len(got) != 0 {
		t.Errorf("without a deployment there are no tenant namespaces, got %v", got)
	}
}
