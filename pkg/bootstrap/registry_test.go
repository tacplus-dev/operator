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
// authorized_services. stratum is the record store that subscribes to eventbus
// topics. (graphdb and timedb were the other two subscribers until the
// 2026-08-25 boneyard removed them from the fleet.)
func TestSubscriberGetsEventbus(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	for _, svc := range []string{"stratum"} {
		deps := sd.authorizedServicesFor(svc)
		has := toSet(deps)
		if !has["eventbus"] {
			t.Errorf("%s deps %v missing 'eventbus'", svc, deps)
		}
	}
}

// TestQueryFrontGetsRecordStore lived here. Its subject was argus, the S2S
// query front for records, which was deleted from the platform 2026-09-01
// (BIG GOAL 8 phase 2 item 9). Nothing else in the fleet resolves a record
// store through a vservice host_label, so the test has no subject to assert
// on — removed rather than repointed at a service that does not exercise that
// resolution.

// TestS2SClientGetsTargets verifies that services with declared s2s client
// dependencies (resolved through host_label for vservice targets) get those
// target services in their authorized_services.
func TestS2SClientGetsTargets(t *testing.T) {
	sd, err := loadServiceDeps()
	if err != nil {
		t.Fatal(err)
	}

	// saml → kernel.Mint. The vservice host_label leg this test also used to
	// cover (argus → stratum.RecordMap, GraphdbQuery, TimedbQuery) lost its
	// last subject when argus was deleted 2026-09-01; every remaining
	// (spec.grpc_s2s_client) in the fleet names a concrete service.
	samlDeps := sd.authorizedServicesFor("saml")
	has := toSet(samlDeps)
	for _, expected := range []string{"kernel"} {
		if !has[expected] {
			t.Errorf("saml deps %v missing %q", samlDeps, expected)
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

// recordReadRegistry is a registry.json shaped the way tacplus emits one TODAY
// for a service that declares a record read against another service's store.
// The vservices/ half is real — the live registry.json already carries
// "query.RecordMap" with host_label "stratum", which is what maps the read
// surface onto the physical record store. The services/ half is the gap: a
// (spec.record_read) annotation produces NO client_dependencies entry, so
// nothing in the node connects mailbox to query.RecordMap.
//
// For TestRecordReadResolvesStratumDelegate to pass, the mailbox node would
// need an entry like
//
//	{"source_package": "mailbox", "surface_kind": "s2s",
//	 "target_package": "query", "target_service": "RecordMap"}
//
// which resolveTarget would then map through host_label to "stratum".
const recordReadRegistry = `{
  "name": "root",
  "children": [
    {
      "name": "query",
      "full_path": "vservices/query",
      "services": [
        {
          "name": "RecordMap",
          "package": "query",
          "grpc_service": "query.RecordMap",
          "host_label": "stratum"
        }
      ]
    },
    {
      "name": "mailbox",
      "full_path": "services/mailbox",
      "services": [
        {
          "name": "Mailbox",
          "package": "mailbox",
          "grpc_service": "mailbox.Mailbox"
        }
      ]
    }
  ]
}`

// TestRecordReadResolvesStratumDelegate is the test Jody asked for: a service
// that declares a record read must resolve a 'stratum' dependency, so its
// kernel delegate authorizes the record store it actually reads from.
//
// EXPECTED RED until tacplus's query.RecordMap client dependency lands.
// tacplus's registry schema has no record-read surface at all today — there is
// no field on ServiceInfo or ClientDependencyInfo that a (spec.record_read)
// annotation lands in — so loadServiceDepsFrom has no input from which to
// derive stratum. This is a missing generator output, tracked in tacplus's own
// inbox, NOT a bug in this repo: do not "fix" it here by hand-deriving stratum
// from something else.
func TestRecordReadResolvesStratumDelegate(t *testing.T) {
	sd, err := loadServiceDepsFrom([]byte(recordReadRegistry))
	if err != nil {
		t.Fatal(err)
	}

	deps := sd.authorizedServicesFor("mailbox")
	if !toSet(deps)["stratum"] {
		t.Errorf("mailbox declares a record read; deps %v missing 'stratum' "+
			"(expected red until tacplus emits the query.RecordMap client dependency)", deps)
	}
}
