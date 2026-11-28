package helm

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// Hyperdisk Balanced, the only disk a C4A (Axion) node attaches, is never
// smaller than 4Gi: a claim that asks for less is not provisioned, and its pod
// waits for good. On seatrials (2026-11-26) kernel's and eventbus's 2Gi claims
// never bound while stratum's 5Gi did. No service's volume may ask for less.
// (Stratum has none now: each tenant's is in memory, #2.)
func TestNoVolumeBelowHyperdiskMinimum(t *testing.T) {
	minimum := resource.MustParse("4Gi")
	g := New("test", "1.2", nil, nil, nil)
	cfg := &protoOperator.Config{
		Topology: &protoOperator.Topology{KernelNodes: 1, EventbusNodes: 1, OauthNodes: 1},
	}
	volumes := 0
	for _, svc := range g.enabledServices(cfg, effectiveTopology(cfg, 1)) {
		if !svc.HasPVC {
			continue
		}
		volumes++
		size, err := resource.ParseQuantity(svc.Storage)
		if err != nil {
			t.Errorf("%s: its volume's size %q is no quantity: %v", svc.Name, svc.Storage, err)
			continue
		}
		if size.Cmp(minimum) < 0 {
			t.Errorf("%s asks for a %s volume: below Hyperdisk Balanced's 4Gi minimum it is never provisioned on a C4A node", svc.Name, svc.Storage)
		}
	}
	if volumes < 3 {
		t.Fatalf("expected kernel, eventbus and redo to have volumes; found %d services with one", volumes)
	}
}
