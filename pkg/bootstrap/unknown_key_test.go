package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// A key in services/<name>.yaml that the service's Config does not declare is
// a typo or a stale field. It must fail the config build, naming the file and
// the key, instead of silently vanishing.
func TestBuildKernelConfigRejectsUnknownServiceKey(t *testing.T) {
	for _, tc := range []struct {
		file string
		yaml string
	}{
		{"gateway.yaml", "listen_adress: \":8443\"\n"},
		{"llmproxy.yaml", "anthropic_workspace_idd: wrkspc_test\n"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &protoOperator.Config{
				Namespace: "workloads",
				Topology:  &protoOperator.Topology{AppServices: map[string]int32{"llmproxy": 1}},
			}
			if err := SaveSettings(dir, cfg); err != nil {
				t.Fatal(err)
			}
			servicesDir := filepath.Join(dir, "services")
			if err := os.MkdirAll(servicesDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("delegates: []\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(servicesDir, tc.file), []byte(tc.yaml), 0644); err != nil {
				t.Fatal(err)
			}

			_, err := BuildKernelConfigProto(dir)
			if err == nil {
				t.Fatalf("services/%s with an unknown key built without error", tc.file)
			}
			key := strings.SplitN(tc.yaml, ":", 2)[0]
			if !strings.Contains(err.Error(), "services/"+tc.file) || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %q, want it to name services/%s and key %q", err, tc.file, key)
			}
		})
	}
}
