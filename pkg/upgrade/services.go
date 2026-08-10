package upgrade

// AllServices returns the complete list of service images that the operator
// may deploy. The checker polls these for new LOCAL_PATCHes.
// proj is the GitLab registry project name; svc is the image name.
func AllServices() []ServiceRef {
	return []ServiceRef{
		{Proj: "kernel", Svc: "kernel"},
		{Proj: "sentinel", Svc: "sentinel"},
		{Proj: "echo", Svc: "echo"},
		{Proj: "eventbus", Svc: "eventbus"},
		{Proj: "gateway", Svc: "gateway"},
		{Proj: "opa", Svc: "opa"},
		{Proj: "argus", Svc: "argus"},
		{Proj: "timedb", Svc: "timedb"},
		{Proj: "graphdb", Svc: "graphdb"},
		{Proj: "stratum", Svc: "stratum"},
		{Proj: "kmaker", Svc: "kmaker"},
		{Proj: "oauth", Svc: "oauth"},
		{Proj: "saml", Svc: "saml"},
		{Proj: "letsencrypt", Svc: "letsencrypt"},
	}
}
