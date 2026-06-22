package bootstrap

var kernelAuthenticatedServiceNames = map[string]bool{
	"sentinel":    true,
	"eventbus":    true,
	"anthropic":   true,
	"oauth":       true,
	"openai":      true,
	"mcp":         true,
	"saml":        true,
	"opa":         true,
	"argus":       true,
	"letsencrypt": true,
	"gateway":     true,
	"timedb":      true,
	"graphdb":     true,
	"stratum":     true,
	"colddb":      true,
	"msggateway":  true,
}

func isKernelAuthenticatedService(name string) bool {
	return kernelAuthenticatedServiceNames[name]
}
