package upgrade

// AllServices returns the complete list of service images that the operator
// may deploy. The checker polls these for new LOCAL_PATCHes.
// proj is the GitLab registry project name; svc is the image name.
func AllServices() []ServiceRef {
	return []ServiceRef{
		{Proj: "kernel", Svc: "kernel"},
		{Proj: "sentinel", Svc: "sentinel"},
		{Proj: "eventbus", Svc: "eventbus"},
		{Proj: "gateway", Svc: "gateway"},
		{Proj: "opa", Svc: "opa"},
		{Proj: "argus", Svc: "argus"},
		{Proj: "timedb", Svc: "clickhouse"},
		{Proj: "graphdb", Svc: "graphdb"},
		{Proj: "stratum", Svc: "stratum"},
		{Proj: "oauth", Svc: "oauth"},
		{Proj: "saml", Svc: "saml"},
		{Proj: "keycloak", Svc: "keycloak"},
		{Proj: "letsencrypt", Svc: "letsencrypt"},
		{Proj: "anthropic-proxy", Svc: "anthropic"},
		{Proj: "openai-proxy", Svc: "openai"},
		{Proj: "mcp", Svc: "mcp"},
		{Proj: "colddb", Svc: "colddb"},
		{Proj: "msggateway", Svc: "msggateway"},
	}
}
