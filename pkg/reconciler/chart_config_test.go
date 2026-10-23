package reconciler

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/helm"
)

// renderedChart is every file of the chart generated for cfg, concatenated in
// file-name order.
func renderedChart(t *testing.T, cfg *protoOperator.Config) string {
	t.Helper()
	charts, err := helm.New("1-2", "1.2", nil, map[string]bool{"dog": true}, nil).Generate(cfg, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(charts.Files))
	for name := range charts.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.Write(charts.Files[name])
	}
	return b.String()
}

// TestChartConfigRendersRecordTenants: a tenant the provider picked up from
// kernel's records gets its tenant-owned dog in the chart beside operator.yaml's
// own tenant, which renders once, and the reconciler's cfg is not changed.
func TestChartConfigRendersRecordTenants(t *testing.T) {
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme"},
		Topology:  &protoOperator.Topology{KernelNodes: 1, AppServices: map[string]int32{"dog": 1}},
	}
	sc := &ServiceConfigs{Tenants: []string{"thescottsweb-com"}}

	chart := renderedChart(t, chartConfig(cfg, sc))

	if !strings.Contains(chart, "name: dog-thescottsweb-com") {
		t.Error("chart renders no dog-thescottsweb-com")
	}
	if n := strings.Count(chart, "name: dog-acme\n"); n != 1 {
		t.Errorf("chart names dog-acme %d times, want 1", n)
	}
	if want := []string{"acme"}; !reflect.DeepEqual(cfg.Tenants, want) {
		t.Errorf("chartConfig changed the reconciler's tenants to %v, want %v", cfg.Tenants, want)
	}
}

// TestChartConfigWithoutRecordTenants: with nothing from kernel's records the
// chart is exactly operator.yaml's.
func TestChartConfigWithoutRecordTenants(t *testing.T) {
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme"},
		Topology:  &protoOperator.Topology{KernelNodes: 1, AppServices: map[string]int32{"dog": 1}},
	}
	want := renderedChart(t, cfg)
	for _, sc := range []*ServiceConfigs{nil, {}} {
		if got := renderedChart(t, chartConfig(cfg, sc)); got != want {
			t.Errorf("chartConfig(cfg, %+v) renders a different chart", sc)
		}
	}
}
