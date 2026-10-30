package bootstrap

import (
	"sort"
	"testing"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

// everyPlatformServiceConfig turns on every optional platform service, so
// enabledServices emits the widest set of serviceSpecs the generator can
// produce. App services are deliberately absent: they are config-declared and
// reach the operator through RegisterAppServices, not through the compile-time
// map this test guards.
func everyPlatformServiceConfig() *protoOperator.Config {
	return &protoOperator.Config{
		Namespace: "schematic",
		Tls:       &protoOperator.TLSConfig{Letsencrypt: true},
		OptionalServices: &protoOperator.OptionalServices{
			Echo:         true,
			Messaging:    true,
			Mailbox:      true,
			Personalauth: true,
			Blobstore:    true,
		},
		Topology: &protoOperator.Topology{
			KernelNodes:        1,
			EventbusNodes:      1,
			GatewayNodes:       1,
			OpaNodes:           1,
			OauthNodes:         1,
			StratumNodes:       1,
			StratumSearchNodes: 1,
			EchoNodes:          1,
			MessagingNodes:     1,
			MailboxNodes:       1,
			PersonalauthNodes:  1,
			BlobstoreNodes:     1,
		},
	}
}

// TestEveryKernelAuthServiceSpecIsInTheMap is the coverage net for the second
// of the three one-line additions a new platform service needs: (1) the
// serviceSpec in the helm generator, (2) a DeployConfigs chassis bootstrap, and
// (3) an entry in kernelAuthenticatedServiceNames. A spec with no map entry gets
// no delegate and no authorized_services, and the service crash-loops on its
// first kernel call — which is how this was found, twice.
func TestEveryKernelAuthServiceSpecIsInTheMap(t *testing.T) {
	cfg := everyPlatformServiceConfig()
	specs := helm.New("test", "1.2", nil, nil, nil).KernelAuthServiceNames(cfg, 1)
	if len(specs) == 0 {
		t.Fatal("the generator emitted no kernel-authenticated serviceSpecs; this test is guarding nothing")
	}

	for _, name := range specs {
		if !kernelAuthenticatedServiceNames[name] {
			t.Errorf("the helm generator renders %q with a kernel identity, but kernelAuthenticatedServiceNames has no entry for it: it will get no delegate and no authorized_services, and crash-loop on its first kernel call. Add %q to the map in services.go.", name, name)
		}
	}
}

// TestEveryMappedKernelAuthServiceIsAServiceSpec is the same net in the other
// direction: a map entry with no serviceSpec means the operator mints a delegate
// for a service nothing deploys, which is how a retired service keeps a live
// kernel identity after its workload is gone.
func TestEveryMappedKernelAuthServiceIsAServiceSpec(t *testing.T) {
	cfg := everyPlatformServiceConfig()
	specs := helm.New("test", "1.2", nil, nil, nil).KernelAuthServiceNames(cfg, 1)
	rendered := make(map[string]bool, len(specs))
	for _, name := range specs {
		rendered[name] = true
	}

	mapped := make([]string, 0, len(kernelAuthenticatedServiceNames))
	for name := range kernelAuthenticatedServiceNames {
		mapped = append(mapped, name)
	}
	sort.Strings(mapped)

	for _, name := range mapped {
		// The operator is the documented exception: it authenticates to kernel
		// like any other service, but its own chart is hand-written and the fleet
		// chart never renders a serviceSpec for it.
		if name == "operator" {
			continue
		}
		if !rendered[name] {
			t.Errorf("kernelAuthenticatedServiceNames has %q, but the helm generator renders no kernel-authenticated serviceSpec for it with every optional service enabled: either the service was retired and the map entry is stale, or everyPlatformServiceConfig does not enable it.", name)
		}
	}
}

// TestDeployedKernelAuthServicesMatchesTheRenderedSpecs pins the third leg: the
// delegate/authorized_services walk and the chart must agree on WHICH of the
// mapped services this config actually deploys, not just that the names exist.
func TestDeployedKernelAuthServicesMatchesTheRenderedSpecs(t *testing.T) {
	cfg := everyPlatformServiceConfig()
	rendered := helm.New("test", "1.2", nil, nil, nil).KernelAuthServiceNames(cfg, 1)

	deployed := make(map[string]bool)
	for _, name := range DeployedKernelAuthServices(cfg) {
		deployed[name] = true
	}
	for _, name := range rendered {
		if !deployed[name] {
			t.Errorf("the chart renders %q with a kernel identity but DeployedKernelAuthServices omits it, so it gets no delegate", name)
		}
	}
	for name := range deployed {
		if name == "operator" {
			continue
		}
		found := false
		for _, r := range rendered {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DeployedKernelAuthServices lists %q but the chart renders no kernel-authenticated workload for it", name)
		}
	}
}
