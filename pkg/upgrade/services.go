package upgrade

// AllServices returns the complete list of service images that the operator
// may deploy. The checker polls these for new LOCAL_PATCHes.
// proj is the GitLab registry project name; svc is the image name.
func AllServices() []ServiceRef {
	return []ServiceRef{
		{Proj: "kernel", Svc: "kernel"},
		// sentinel went to the boneyard 2026-10-12 (sonar mvp-plan "Amendment
		// 2026-10-12"; it returns later as part of the Admiral): out of the fleet,
		// proto deleted, no image published.
		{Proj: "echo", Svc: "echo"},
		{Proj: "eventbus", Svc: "eventbus"},
		{Proj: "gateway", Svc: "gateway"},
		{Proj: "opa", Svc: "opa"},
	// timedb and graphdb went to the boneyard 2026-08-25 (BIG GOAL 3, first
	// move #1): out of the fleet build, protos deleted, no image published.
		{Proj: "stratum", Svc: "stratum"},
		{Proj: "kmaker", Svc: "kmaker"},
		{Proj: "oauth", Svc: "oauth"},
		{Proj: "letsencrypt", Svc: "letsencrypt"},
	}
}
