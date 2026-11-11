// Package deployment names what one deployment of the fleet owns on a
// Kubernetes cluster that several share (decisions/deployments-per-cluster-2026-10-26.md
// in tacplus-dev/roadmap, #3): its platform namespace tac-<deployment>, one
// tenant namespace tac-<deployment>-<tid> per tenant, and the in-cluster names
// of its services. Both the chart generator and the config builder name things
// through here, so the two cannot disagree.
package deployment

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/util/validation"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// MaxNameLength is the longest deployment name: tac-<16>-<8> is 29
// characters, well inside a namespace's 63.
const MaxNameLength = 16

// NamespacePrefix starts every namespace a deployment owns.
const NamespacePrefix = "tac-"

// LegacyNamespace is Config.namespace's default: the one namespace a fleet
// used before deployments.
const LegacyNamespace = "schematic"

// The reserved tenants every deployment has, and their fixed tids. All-digit
// tids are the platform's; a minted tid starts with a letter (kernel#3).
const (
	PeopleTenant  = "people"
	PeopleTid     = "00000001"
	RestoreTenant = "restore"
	RestoreTid    = "00000002"
)

// reservedTids are the reserved tenants' tids, by tenant name.
var reservedTids = map[string]string{PeopleTenant: PeopleTid, RestoreTenant: RestoreTid}

// DeploymentLabel is the label the top pipeline sets on tac-<deployment>;
// deploymentproxy reads the deployment from it, never from the name.
const DeploymentLabel = "tacplus.dev/deployment"

// deploymentproxy's labels on a tenant namespace it made (deploymentproxy
// v0.2.0). kernel's Raft NetworkPolicy admits the deployment's tenant
// namespaces to kernel's Hazelcast port by OwnerLabel.
const (
	OwnerLabel = "deploymentproxy.tacplus.dev/deployment"
	TidLabel   = "deploymentproxy.tacplus.dev/tid"
)

var (
	nameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	tidRE  = regexp.MustCompile(`^[0-9a-z]{8}$`)
	digits = regexp.MustCompile(`^[0-9]+$`)
)

// ValidateName checks a deployment name: at most 16 characters of [a-z0-9-],
// starting with a letter, and ending with a letter or digit, since it ends the
// platform namespace's name.
func ValidateName(name string) error {
	if len(name) == 0 || len(name) > MaxNameLength {
		return fmt.Errorf("deployment %q: must be 1 to %d characters", name, MaxNameLength)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("deployment %q: must be [a-z0-9-], start with a letter and end with a letter or digit", name)
	}
	return nil
}

// ValidateTid checks a tenant id's shape: exactly 8 of [0-9a-z].
func ValidateTid(tid string) error {
	if !tidRE.MatchString(tid) {
		return fmt.Errorf("tid %q: must be exactly 8 of [0-9a-z]", tid)
	}
	return nil
}

// Namespace is the platform namespace of deployment name.
func Namespace(name string) string {
	return NamespacePrefix + name
}

// TenantNamespace is the namespace of tenant tid in deployment name.
func TenantNamespace(name, tid string) string {
	return Namespace(name) + "-" + tid
}

// ServiceHost is the full in-cluster DNS name of host (a Service, or a
// <pod>.<headless Service>) in namespace ns. Every endpoint the operator writes
// uses it: a tenant namespace's service must reach its deployment's platform
// services, and a short name resolves only inside its own namespace.
func ServiceHost(host, ns string) string {
	return host + "." + ns + ".svc.cluster.local"
}

// PlatformNamespace is where cfg's platform services run: tac-<deployment>
// when cfg names a deployment, otherwise its namespace (default schematic).
func PlatformNamespace(cfg *protoOperator.Config) string {
	if d := cfg.GetDeployment(); d != "" {
		return Namespace(d)
	}
	if ns := cfg.GetNamespace(); ns != "" {
		return ns
	}
	return LegacyNamespace
}

// TenantNamespaceOf is where tenant's tenant-owned services run: its tenant
// namespace when cfg names a deployment, otherwise the one namespace. A tenant
// with no tid has none; Validate refuses that config.
func TenantNamespaceOf(cfg *protoOperator.Config, tenant string) string {
	d := cfg.GetDeployment()
	if d == "" {
		return PlatformNamespace(cfg)
	}
	return TenantNamespace(d, TenantTids(cfg)[tenant])
}

// TenantTids returns, by tenant name, the tid of every tenant that gets a
// tenant namespace: each of cfg's tenants and the reserved people and
// restore. Empty when cfg names no deployment: there are no tenant namespaces
// then.
func TenantTids(cfg *protoOperator.Config) map[string]string {
	if cfg.GetDeployment() == "" {
		return map[string]string{}
	}
	out := map[string]string{}
	for _, t := range cfg.GetTenants() {
		if tid, ok := cfg.GetTenantIds()[t]; ok {
			out[t] = tid
		}
	}
	// The reserved tenants have theirs in every deployment, listed or not.
	for t, tid := range reservedTids {
		out[t] = tid
	}
	return out
}

// Validate checks cfg's deployment, namespace and tenant ids together:
//   - deployment, when set, is a valid name, and namespace is unset, its
//     default, or tac-<deployment>;
//   - every tid is a valid one, not all digits, and no two tenants share one;
//   - a tid names a listed tenant; a reserved tenant (people, restore) may be
//     listed, and its tid, if given, is its reserved one;
//   - with a deployment, every other tenant has a tid, and every namespace it
//     makes is a valid namespace name.
func Validate(cfg *protoOperator.Config) error {
	var errs []error
	d := cfg.GetDeployment()
	if d != "" {
		if err := ValidateName(d); err != nil {
			return err
		}
		if ns := cfg.GetNamespace(); ns != "" && ns != LegacyNamespace && ns != Namespace(d) {
			errs = append(errs, fmt.Errorf("namespace %q: a deployment's namespace is %s; leave namespace unset", ns, Namespace(d)))
		}
	}
	listed := make(map[string]bool, len(cfg.GetTenants()))
	for _, t := range cfg.GetTenants() {
		listed[t] = true
		if _, ok := cfg.GetTenantIds()[t]; !ok && d != "" && reservedTids[t] == "" {
			errs = append(errs, fmt.Errorf("tenant %q has no tid: tenant_ids must give it one (its namespace is tac-%s-<tid>)", t, d))
		}
	}
	owner := map[string]string{}
	names := make([]string, 0, len(cfg.GetTenantIds()))
	for t := range cfg.GetTenantIds() {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		tid := cfg.GetTenantIds()[t]
		if !listed[t] {
			errs = append(errs, fmt.Errorf("tenant_ids names %q, which tenants does not list", t))
			continue
		}
		if err := ValidateTid(tid); err != nil {
			errs = append(errs, fmt.Errorf("tenant %q: %w", t, err))
			continue
		}
		if reserved := reservedTids[t]; reserved != "" && tid != reserved {
			errs = append(errs, fmt.Errorf("tenant %q is reserved: its tid is %s in every deployment, not %s", t, reserved, tid))
			continue
		}
		if digits.MatchString(tid) && reservedTids[t] == "" {
			errs = append(errs, fmt.Errorf("tenant %q: tid %s is all digits, which is reserved for the platform", t, tid))
			continue
		}
		if other, dup := owner[tid]; dup {
			errs = append(errs, fmt.Errorf("tenants %q and %q share tid %s", other, t, tid))
			continue
		}
		owner[tid] = t
		if d != "" {
			if bad := validation.IsDNS1123Label(TenantNamespace(d, tid)); len(bad) > 0 {
				errs = append(errs, fmt.Errorf("tenant %q: namespace %s: %v", t, TenantNamespace(d, tid), bad))
			}
		}
	}
	return errors.Join(errs...)
}
