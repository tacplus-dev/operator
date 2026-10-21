package bootstrap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoDiskstore "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/diskstore"
	protoGrpcConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/sys/grpcconfig"
)

// stratumArchiveTestConfig is a fleet with three writers and one search member,
// the shape the archive is enabled on.
func stratumArchiveTestConfig(t *testing.T) *protoOperator.Config {
	t.Helper()
	cfg := &protoOperator.Config{
		Namespace:    "schematic",
		Fqdn:         "console.seabeam.cloud",
		SpiffePrefix: "local.dev",
		Topology: &protoOperator.Topology{
			KernelNodes:        1,
			StratumNodes:       3,
			StratumSearchNodes: 1,
		},
	}
	if err := GenerateCrypto(cfg, 1, 0, cfg.Fqdn, cfg.SpiffePrefix); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// stratumConfigs saves cfg to dir and returns the bootstrap config under every
// values key, from both paths that feed the chart: the reconciler's
// (ServiceConfigValues) and `apply --bundle`'s (BundleServiceValues). They must
// agree, so a mismatch fails here.
func stratumConfigs(t *testing.T, dir string, cfg *protoOperator.Config) map[string]string {
	t.Helper()
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := SaveCrypto(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "services"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "kernel.yaml"), []byte("{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dc, err := BuildDeployConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	fromBundle := BundleServiceValues(bundle)

	out := map[string]string{}
	for key, v := range ServiceConfigValues(dc) {
		got, _ := v.(map[string]interface{})["config"].(string)
		want, _ := fromBundle[key].(map[string]interface{})["config"].(string)
		if got != want {
			t.Errorf("%s: ServiceConfigValues and BundleServiceValues disagree", key)
		}
		out[key] = got
	}
	return out
}

func TestStratumDiskPersistenceRendersOnWritersOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := stratumArchiveTestConfig(t)
	before := stratumConfigs(t, dir, cfg)

	cfg.StratumDiskPersistence = &protoDiskstore.DiskPersistenceConfig{
		Dir:                 "/app/data",
		RollIntervalSeconds: 60,
		Compression:         protoDiskstore.Compression_COMPRESSION_GZ,
		Archive: &protoDiskstore.ArchiveConfig{
			GcsBucket:                 "jody1-328023-datagate-backups",
			Prefix:                    "stratum",
			CheckpointIntervalSeconds: 86400,
		},
	}
	after := stratumConfigs(t, dir, cfg)

	writer := after["stratum"]
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(writer), &fields); err != nil {
		t.Fatal(err)
	}
	// The shape stratum's HotArchiveConfigTest loads. compression is the
	// generated helper's short enum form, which stratum's generated
	// parseCompression reads as COMPRESSION_GZ (checked below on the Go side).
	want := map[string]any{
		"dir":                   "/app/data",
		"roll_interval_seconds": 60,
		"compression":           "gz",
		"archive": map[string]any{
			"gcs_bucket":                  "jody1-328023-datagate-backups",
			"prefix":                      "stratum",
			"checkpoint_interval_seconds": 86400,
		},
	}
	if !reflect.DeepEqual(fields["disk_persistence"], want) {
		t.Fatalf("writer disk_persistence = %#v, want %#v\n--- stratum config ---\n%s", fields["disk_persistence"], want, writer)
	}
	parsed, err := protoGrpcConfig.ConfigFromBytes([]byte(writer))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.GetDiskPersistence().GetCompression(); got != protoDiskstore.Compression_COMPRESSION_GZ {
		t.Errorf("writer compression parses as %v, want COMPRESSION_GZ", got)
	}

	// The search member persists nothing and has no volume: its bootstrap is
	// exactly what every stratum pod got before the field existed.
	search, ok := after["stratum-search"]
	if !ok {
		t.Fatalf("no stratum-search config: the search member would read the writers' disk_persistence")
	}
	if search != before["stratum"] {
		t.Errorf("stratum-search config changed\n--- before ---\n%s\n--- after ---\n%s", before["stratum"], search)
	}
}

func TestStratumWithoutDiskPersistenceRendersNone(t *testing.T) {
	configs := stratumConfigs(t, t.TempDir(), stratumArchiveTestConfig(t))

	for key, c := range configs {
		if strings.Contains(c, "disk_persistence") {
			t.Errorf("%s carries disk_persistence with the field unset\n%s", key, c)
		}
	}
	search, ok := configs["stratum-search"]
	if !ok {
		t.Fatal("no stratum-search config")
	}
	if search != configs["stratum"] {
		t.Errorf("with the field unset, stratum-search and stratum configs differ\n--- stratum ---\n%s\n--- stratum-search ---\n%s",
			configs["stratum"], search)
	}
}
