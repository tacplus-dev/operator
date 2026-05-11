package bootstrap

// FrontendServiceNames lists services that need credential pairs and kernel
// delegates for service-to-service token exchange.
var FrontendServiceNames = []string{
	"sentinel",
	"eventbus",
	"anthropic",
	"oauth",
	"openai",
	"mcp",
	"saml",
	"opa",
	"argus",
	"letsencrypt",
	"keycloak",
}
