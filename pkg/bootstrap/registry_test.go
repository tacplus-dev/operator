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

// A service that watches an eventbus topic gets the broker in its
// authorized_services. Driven from a fixture registry: which fleet services
// watch a topic is the fleet's business, and this library is generated against
// whichever fleet builds it (the proof fleet has no watcher at all).
func TestWatcherGetsBroker(t *testing.T) {
	sd, err := loadServiceDepsFrom([]byte(`{
  "name": "root",
  "children": [
    {"name": "eventbus", "full_path": "services/eventbus",
     "services": [{"name": "Admin", "package": "eventbus", "grpc_service": "eventbus.Admin"}]},
    {"name": "oauth", "full_path": "services/oauth",
     "services": [{"name": "Auth", "package": "oauth", "grpc_service": "oauth.Auth"}],
     "event_watches": [{"broker_label": "eventbus"}]}
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	deps := sd.authorizedServicesFor("oauth")
	if !toSet(deps)["eventbus"] {
		t.Errorf("oauth deps %v missing 'eventbus' (it watches a topic the broker carries)", deps)
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

	// personalauth → kernel.Mint. The vservice host_label leg this test also used to
	// cover (argus → stratum.RecordMap, GraphdbQuery, TimedbQuery) lost its
	// last subject when argus was deleted 2026-09-01; every remaining
	// (spec.grpc_s2s_client) in the fleet names a concrete service.
	personalauthDeps := sd.authorizedServicesFor("personalauth")
	has := toSet(personalauthDeps)
	for _, expected := range []string{"kernel"} {
		if !has[expected] {
			t.Errorf("personalauth deps %v missing %q", personalauthDeps, expected)
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

// recordReadRegistry is a registry.json shaped the way tacplus emits one for a
// service that declares a record read against another service's store: the
// vservices/ half carries "query.RecordMap" with host_label "stratum" (which
// maps the read surface onto the physical record store), and the services/
// half carries the client_dependencies entry tacplus derives from
// (spec.record_read) — landed in tacplus@45b5db9f, confirmed present in a real
// generate as of 2026-09-27 (mailbox/letsencrypt/blobstore's own
// (spec.record_read) annotations each now produce exactly this entry).
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
      ],
      "client_dependencies": [
        {
          "source_package": "mailbox",
          "surface_kind": "s2s",
          "target_package": "query",
          "target_service": "RecordMap"
        }
      ]
    }
  ]
}`

// TestRecordReadResolvesStratumDelegate is the test Jody asked for: a service
// that declares a record read must resolve a 'stratum' dependency, so its
// kernel delegate authorizes the record store it actually reads from.
//
// tacplus@45b5db9f added the client_dependencies entry a (spec.record_read)
// annotation produces; confirmed against a real generate of platform/proto
// (mailbox, letsencrypt, blobstore) on 2026-09-27 that authorizedServicesFor
// resolves 'stratum' for all three via this exact path.
func TestRecordReadResolvesStratumDelegate(t *testing.T) {
	sd, err := loadServiceDepsFrom([]byte(recordReadRegistry))
	if err != nil {
		t.Fatal(err)
	}

	deps := sd.authorizedServicesFor("mailbox")
	if !toSet(deps)["stratum"] {
		t.Errorf("mailbox declares a record read; deps %v missing 'stratum'", deps)
	}
}
