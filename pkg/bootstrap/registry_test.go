package bootstrap

import (
	"sort"
	"testing"
)

// TestLoadServiceDepsFromRegistry verifies the registry-derived dependency
// graph for services with known cross-service requirements.
func TestLoadServiceDepsFromRegistry(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	// Every service must include itself and kernel.
	for svc, deps := range sd.deps {
		has := toSet(deps)
		if !has[svc] {
			t.Errorf("service %q missing itself in deps: %v", svc, deps)
		}
		if svc != kernelServiceName && !has[kernelServiceName] {
			t.Errorf("service %q missing kernel in deps: %v", svc, deps)
		}
	}
}

// TestSubscriberGetsEventbus verifies that services whose config state is
// hosted by eventbus (tag: "eventbus" on boundaries) get eventbus in their
// authorized_services. graphdb and timedb are projection stores that subscribe
// to eventbus topics.
func TestSubscriberGetsEventbus(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	for _, svc := range []string{"graphdb", "timedb"} {
		deps := sd.authorizedServicesFor(svc)
		has := toSet(deps)
		if !has["eventbus"] {
			t.Errorf("%s deps %v missing 'eventbus'", svc, deps)
		}
	}
}

// TestQueryFrontGetsRecordStore verifies that argus (the S2S query front for
// records) gets stratum (the record store) in its authorized_services via the
// agentsensor.StratumQuery host_tag resolution.
func TestQueryFrontGetsRecordStore(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	deps := sd.authorizedServicesFor("argus")
	has := toSet(deps)
	if !has["stratum"] {
		t.Errorf("argus deps %v missing 'stratum'", deps)
	}
}

// TestS2SClientGetsTargets verifies that services with declared s2s client
// dependencies (resolved through host_tag for vservice targets) get those
// target services in their authorized_services.
func TestS2SClientGetsTargets(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	// argus → agentsensor.StratumQuery (host_tag: stratum),
	//         agentsensor.GraphdbQuery (host_tag: graphdb),
	//         agentsensor.TimedbQuery (host_tag: timedb)
	argusDeps := sd.authorizedServicesFor("argus")
	has := toSet(argusDeps)
	for _, expected := range []string{"stratum", "graphdb", "timedb"} {
		if !has[expected] {
			t.Errorf("argus deps %v missing %q", argusDeps, expected)
		}
	}

	// stratum → agentsensor.GraphdbQuery, agentsensor.TimedbQuery
	stratumDeps := sd.authorizedServicesFor("stratum")
	has = toSet(stratumDeps)
	for _, expected := range []string{"graphdb", "timedb"} {
		if !has[expected] {
			t.Errorf("stratum deps %v missing %q", stratumDeps, expected)
		}
	}
}

// TestNeverANY verifies no service is authorized for SERVICE_NAME_ANY.
func TestNeverANY(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	for svc, deps := range sd.deps {
		for _, d := range deps {
			if d == "ANY" || d == "SERVICE_NAME_ANY" {
				t.Errorf("service %q has forbidden dep %q: %v", svc, d, deps)
			}
		}
	}
}

// TestIdempotentLoadServiceDeps verifies that loading twice produces identical
// results (deterministic — all outputs sorted).
func TestIdempotentLoadServiceDeps(t *testing.T) {
	sd1, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}
	sd2, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	if len(sd1.deps) != len(sd2.deps) {
		t.Fatalf("different number of services: %d vs %d", len(sd1.deps), len(sd2.deps))
	}

	// Get sorted service list for deterministic comparison.
	svcs := make([]string, 0, len(sd1.deps))
	for s := range sd1.deps {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)

	for _, svc := range svcs {
		d1 := sd1.deps[svc]
		d2 := sd2.deps[svc]
		if len(d1) != len(d2) {
			t.Fatalf("service %q: %v vs %v", svc, d1, d2)
		}
		for i := range d1 {
			if d1[i] != d2[i] {
				t.Fatalf("service %q: %v vs %v", svc, d1, d2)
			}
		}
	}
}

// TestFallbackForUnknownService verifies that a service not in the registry
// falls back to the minimum [self, kernel] pair.
func TestFallbackForUnknownService(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	deps := sd.authorizedServicesFor("unknown-future-svc")
	has := toSet(deps)
	if !has["unknown-future-svc"] {
		t.Errorf("fallback deps missing self: %v", deps)
	}
	if !has[kernelServiceName] {
		t.Errorf("fallback deps missing kernel: %v", deps)
	}
}

func toSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}
