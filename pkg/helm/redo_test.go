package helm

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// The redo service is Stratum's durability (decisions/stratum-per-tenant-2026-10-25.md
// in tacplus-dev/roadmap): one instance, redo-restore, a tenant service of the
// reserved restore tenant (Jody, 2026-10-26), so it lands in that tenant's
// namespace tac-<deployment>-00000002, never in tac-<deployment>.
func decodeStatefulSet(t *testing.T, o renderedObject) appsv1.StatefulSet {
	t.Helper()
	var ss appsv1.StatefulSet
	if err := k8syaml.Unmarshal([]byte(o.Doc), &ss); err != nil {
		t.Fatalf("decoding %s: %v\n%s", o.key(), err, o.Doc)
	}
	return ss
}

func TestRedoRendersInTheRestoreTenantNamespace(t *testing.T) {
	cfg := deploymentConfig("prod")
	cfg.RedoStorageClass = "regional-pd"
	objs := renderedObjects(t, generateDeployment(t, cfg))

	const ns = "tac-prod-00000002"
	ss := decodeStatefulSet(t, findObject(t, objs, "StatefulSet", ns, "redo-restore"))
	findObject(t, objs, "ServiceAccount", ns, "redo-restore")
	findObject(t, objs, "Service", ns, "redo-restore-0")

	if ss.Spec.Replicas == nil || *ss.Spec.Replicas != 1 {
		t.Errorf("redo-restore replicas = %v, want 1: redo is pinned, never replicated", ss.Spec.Replicas)
	}
	// Its volume is the regional disk a local ack is durable on: a claim on the
	// StorageClass the cluster pipeline provides, mounted at redo's data_dir.
	if n := len(ss.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("redo-restore volumeClaimTemplates = %d, want 1", n)
	}
	if sc := ss.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "regional-pd" {
		t.Errorf("redo-restore storageClassName = %v, want regional-pd", sc)
	}
	var mounted bool
	for _, m := range ss.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "data" {
			mounted = true
			if m.MountPath != RedoDataDir {
				t.Errorf("redo-restore data mounted at %s, want %s (redo.Config.data_dir)", m.MountPath, RedoDataDir)
			}
		}
	}
	if !mounted {
		t.Error("redo-restore mounts no data volume")
	}

	for _, o := range objs {
		if strings.HasPrefix(o.Name, "redo") && o.Namespace != ns {
			t.Errorf("%s rendered outside the restore tenant's namespace %s", o.key(), ns)
		}
	}
}

// Unset, the claim names no class and the cluster's default applies.
func TestRedoWithoutStorageClassTakesTheClusterDefault(t *testing.T) {
	objs := renderedObjects(t, generateDeployment(t, deploymentConfig("prod")))
	ss := decodeStatefulSet(t, findObject(t, objs, "StatefulSet", "tac-prod-00000002", "redo-restore"))
	if sc := ss.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc != nil {
		t.Errorf("redo-restore storageClassName = %q, want none", *sc)
	}
}

// In the one-namespace layout every tenant shares the namespace, restore too.
func TestRedoInTheOneNamespaceLayout(t *testing.T) {
	cfg := deploymentConfig("")
	cfg.Namespace = "schematic"
	objs := renderedObjects(t, generateDeployment(t, cfg))
	findObject(t, objs, "StatefulSet", "schematic", "redo-restore")
}

// redo exists for Stratum: a fleet that renders no Stratum renders no redo.
func TestNoRedoWithoutStratum(t *testing.T) {
	cfg := deploymentConfig("prod")
	cfg.Topology.StratumNodes = 0
	for _, o := range renderedObjects(t, generateDeployment(t, cfg)) {
		if strings.HasPrefix(o.Name, "redo") {
			t.Errorf("%s rendered with no Stratum", o.key())
		}
	}
}
