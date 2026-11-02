package helm

import (
	"strings"
	"testing"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// Without SCHEMATIC_LOCAL_IMAGES the chart pulls each service's released
// image from its tacplus-dev home, ghcr.io/tacplus-dev/<service>, the image
// its release publishes (tacplus-dev/tacplus#180). A pinned digest replaces
// the tag there too.
func TestReleasedImagesComeFromTacplusDev(t *testing.T) {
	t.Setenv("SCHEMATIC_LOCAL_IMAGES", "")
	digest := "sha256:" + strings.Repeat("ab", 32)
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			OpaNodes:    1,
		},
		ImageDigests: map[string]string{"kernel": digest},
	})

	values := chartFile(t, charts, "values.yaml")
	for _, want := range []string{
		"kernel:\n  image: ghcr.io/tacplus-dev/kernel@" + digest + "\n",
		"opa:\n  image: ghcr.io/tacplus-dev/opa:latest\n",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml missing %q\n--- values.yaml ---\n%s", want, values)
		}
	}
}

// The node-pool and pod labels are tacplus.dev's: the cluster that provisions
// the pools labels and taints its nodes with NodePoolLabel.
func TestNodeLabelsAreTacplusDev(t *testing.T) {
	if NodePoolLabel != "tacplus.dev/pool" {
		t.Errorf("NodePoolLabel = %q, want tacplus.dev/pool", NodePoolLabel)
	}
	if ServicePodLabel != "tacplus.dev/service" {
		t.Errorf("ServicePodLabel = %q, want tacplus.dev/service", ServicePodLabel)
	}
}
