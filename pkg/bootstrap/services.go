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
	"argus":       true,
	"letsencrypt": true,
	"gateway":     true,
	"timedb":      true,
	"graphdb":     true,
	"stratum":     true,
	"echo":        true,
}

func isKernelAuthenticatedService(name string) bool {
	return kernelAuthenticatedServiceNames[name]
}

// alwaysOnKernelAuthServices are kernel-authenticated services that are always
// deployed (their replica counts come from topology presets and are always > 0).
// Sorted for deterministic output.
var alwaysOnKernelAuthServices = []string{
	"argus",
	"eventbus",
	"gateway",
	"graphdb",
	"oauth",
	"opa",
	"sentinel",
	"stratum",
	"timedb",
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

	sort.Strings(out)
	return out
}
