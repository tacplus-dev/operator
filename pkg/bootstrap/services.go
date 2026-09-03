package bootstrap

import (
	"sort"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

var kernelAuthenticatedServiceNames = map[string]bool{
	"sentinel":    true,
	"eventbus":    true,
	"oauth":       true,
	"saml":        true,
	"opa":         true,
	"letsencrypt": true,
	"gateway":     true,
	// The operator authenticates to kernel like any other service: it holds a
	// kernel identity so its reconciler can push the OPA config into a running
	// cluster (kernel.Mint/UpdateOpaConfig). It is not deployed BY the fleet
	// chart -- it deploys that chart -- but from kernel's point of view it is
	// an ordinary delegate.
	"operator":    true,
	// timedb and graphdb went to the boneyard 2026-08-25 (BIG GOAL 3, first
	// move #1): out of the fleet build, protos deleted, no image published.
	"stratum":     true,
	"echo":        true,
	"messaging":   true,
	"mailbox":     true,
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

// delegateServiceAccountName returns the Kubernetes ServiceAccount that
// authenticates as svc. For every service the operator deploys this is the
// service's own name, because the generated chart names the ServiceAccount
// after the service. The operator is the exception: its own chart is
// hand-written and names its ServiceAccount "schematic-operator".
func delegateServiceAccountName(svc string) string {
	if svc == "operator" {
		return operatorServiceAccountName
	}
	return svc
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
	// The application's own services, declared by name rather than by a proto
	// field the platform had to add for them.
	out = append(out, AppServiceNames(cfg)...)

	sort.Strings(out)
	return out
}
