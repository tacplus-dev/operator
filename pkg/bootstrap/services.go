package bootstrap

import (
	"sort"
	"strings"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
)

var kernelAuthenticatedServiceNames = map[string]bool{
	"sentinel":    true,
	"eventbus":    true,
	"oauth":       true,
	"saml":        true,
	"opa":         true,
	"letsencrypt": true,
	"gateway":     true,
	// The operator authenticates to kernel like any other service: it is not
	// deployed BY the fleet chart -- it deploys that chart -- but from kernel's
	// point of view it is an ordinary delegate. The live opa config push this
	// identity was introduced for was retired 2026-09-24; the identity itself
	// is left registered.
	"operator": true,
	// timedb and graphdb went to the boneyard 2026-08-25 (BIG GOAL 3, first
	// move #1): out of the fleet build, protos deleted, no image published.
	"stratum":      true,
	"echo":         true,
	"messaging":    true,
	"mailbox":      true,
	"personalauth": true,
}

// appServiceNames holds the application's own services, seeded from
// Topology.app_services before the dependency graph is walked.
//
// The map above lists PLATFORM services. An app's services are not known at
// compile time — that is the entire point of app_services — but they are
// kernel-authenticated exactly like a platform service, because they are
// generated against the same ServiceManager. loadServiceDeps walks the merged
// proto registry with no access to config, so the set is registered rather than
// threaded through every caller.
var appServiceNames = map[string]bool{}

// RegisterAppServices records the application's service names. Safe to call
// repeatedly; each call replaces the previous set.
func RegisterAppServices(names []string) {
	appServiceNames = make(map[string]bool, len(names))
	for _, n := range names {
		appServiceNames[n] = true
	}
}

// AppServiceNames returns the sorted application service names declared in cfg.
// Presence with a replica count > 0 is the enablement; app services carry no
// parallel optional_services flag.
func AppServiceNames(cfg *protoOperator.Config) []string {
	var out []string
	for name, n := range cfg.GetTopology().GetAppServices() {
		if n > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func isKernelAuthenticatedService(name string) bool {
	return kernelAuthenticatedServiceNames[name] || appServiceNames[name]
}

// alwaysOnKernelAuthServices are kernel-authenticated services that are always
// deployed (their replica counts come from topology presets and are always > 0).
// Sorted for deterministic output.
var alwaysOnKernelAuthServices = []string{
	"eventbus",
	"gateway",
	"oauth",
	"opa",
	"operator",
	"sentinel",
	"stratum",
}

// operatorServiceAccountName is the Kubernetes ServiceAccount the operator pod
// runs as, from the operator's own chart (helm/templates/serviceaccount.yaml).
const operatorServiceAccountName = "schematic-operator"

// ServiceInstance is one deployed instance of a kernel-authenticated service.
//
// A shared service has exactly one instance, with an empty Tenant. A service
// registry.json marks (spec.service).tenant_owned has one instance per entry in
// Config.tenants — the operator runs a separate pod, ServiceAccount and kernel
// delegate for each, so N tenants never collapse into one.
type ServiceInstance struct {
	// Service is the service name, e.g. "docmanager". It is also the kernel
	// delegate's name for every tenant: the delegate name is the service path
	// that auth_services patterns match, so it must not carry the tenant
	// (kernel keys delegates by the (name, tenant_namespace) pair instead).
	Service string
	// Tenant is the bare tenant name, e.g. "acme". Empty for a shared service.
	Tenant string
}

// InstanceName is the Kubernetes Deployment/ServiceAccount name for this
// instance: the service name for a shared service, "<svc>-<tenant>" otherwise.
func (si ServiceInstance) InstanceName() string {
	if si.Tenant == "" {
		return si.Service
	}
	return helm.TenantInstanceName(si.Service, si.Tenant)
}

// TenantNamespace is the canonical authorization namespace written to the
// kernel delegate's tenant_namespace field. Empty for a shared service, which
// kernel reads as the /system root.
func (si ServiceInstance) TenantNamespace() string {
	if si.Tenant == "" {
		return ""
	}
	return tenantNamespacePrefix + si.Tenant
}

const tenantNamespacePrefix = "/tenants/"

// tenantFromNamespace turns a canonical "/tenants/<name>" authorization
// namespace back into the bare tenant name. Empty (the shared /system case)
// stays empty.
func tenantFromNamespace(ns string) string {
	return strings.TrimPrefix(ns, tenantNamespacePrefix)
}

// DeployedServiceInstances returns every (service, tenant) instance the
// operator deploys for cfg, in the order DeployedKernelAuthServices lists the
// services and Config.tenants lists the tenants.
//
// Whether a service is tenant-owned comes from the registry.json annotation
// alone — there is deliberately no operator-side allowlist that could disagree
// with it.
func DeployedServiceInstances(cfg *protoOperator.Config) ([]ServiceInstance, error) {
	tenantOwned, err := tenantOwnedServices()
	if err != nil {
		return nil, err
	}
	pinnedTenant, err := pinnedTenantServices()
	if err != nil {
		return nil, err
	}
	var out []ServiceInstance
	for _, svc := range DeployedKernelAuthServices(cfg) {
		out = append(out, serviceInstances(cfg, svc, tenantOwned, pinnedTenant)...)
	}
	return out, nil
}

// serviceInstances expands one service name into the instances the operator
// deploys for it: exactly one for a shared service, one per Config.tenants
// entry for a tenant-owned one, and exactly one — in the pinned tenant — for a
// tenant-owned service registry.json pins with (spec.service).pinned_tenant.
// The helm generator refuses to render at all when a pinned tenant is not
// configured, so nothing here has to second-guess the pin.
func serviceInstances(cfg *protoOperator.Config, svc string, tenantOwned map[string]bool, pinnedTenant map[string]string) []ServiceInstance {
	if !tenantOwned[svc] {
		return []ServiceInstance{{Service: svc}}
	}
	if tenant, pinned := pinnedTenant[svc]; pinned {
		return []ServiceInstance{{Service: svc, Tenant: tenant}}
	}
	out := make([]ServiceInstance, 0, len(cfg.GetTenants()))
	for _, tenant := range cfg.GetTenants() {
		out = append(out, ServiceInstance{Service: svc, Tenant: tenant})
	}
	return out
}

// delegateServiceAccountName returns the Kubernetes ServiceAccount that
// authenticates as si. For every service the operator deploys this is the
// instance's own name, because the generated chart names the ServiceAccount
// after the workload. The operator is the exception: its own chart is
// hand-written and names its ServiceAccount "schematic-operator".
func delegateServiceAccountName(si ServiceInstance) string {
	if si.Service == "operator" {
		return operatorServiceAccountName
	}
	return si.InstanceName()
}

// DeployedKernelAuthServices returns the sorted list of kernel-authenticated
// service names that would be deployed for the given operator config. This
// mirrors the conditional logic in the helm generator's enabledServices.
func DeployedKernelAuthServices(cfg *protoOperator.Config) []string {
	out := make([]string, len(alwaysOnKernelAuthServices))
	copy(out, alwaysOnKernelAuthServices)

	opt := cfg.GetOptionalServices()
	topo := cfg.GetTopology()

	if opt.GetSaml() {
		out = append(out, "saml")
	}
	if cfg.GetTls().GetLetsencrypt() {
		out = append(out, "letsencrypt")
	}
	if opt.GetEcho() && topo.GetEchoNodes() > 0 {
		out = append(out, "echo")
	}
	if opt.GetMessaging() && topo.GetMessagingNodes() > 0 {
		out = append(out, "messaging")
	}
	if opt.GetMailbox() && topo.GetMailboxNodes() > 0 {
		out = append(out, "mailbox")
	}
	if opt.GetPersonalauth() && topo.GetPersonalauthNodes() > 0 {
		out = append(out, "personalauth")
	}
	// The application's own services, declared by name rather than by a proto
	// field the platform had to add for them.
	out = append(out, AppServiceNames(cfg)...)

	sort.Strings(out)
	return out
}
