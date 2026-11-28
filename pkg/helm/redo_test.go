package helm

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	k8syaml "sigs.k8s.io/yaml"
)

// The redo service is Stratum's durability (decisions/stratum-per-tenant-2026-10-25.md
// in tacplus-dev/roadmap): one /system instance, redo, in the platform
// namespace tac-<deployment>, never in a tenant's namespace (Jody, 2026-11-07,
// decisions/s2s-never-crosses-tenants-2026-10-31.md, addendum: "It leaves the
// restore tenant"). Every tenant's Stratum calls it, tenant -> /system.
func decodeStatefulSet(t *testing.T, o renderedObject) appsv1.StatefulSet {
	t.Helper()
	var ss appsv1.StatefulSet
	if err := k8syaml.Unmarshal([]byte(o.Doc), &ss); err != nil {
		t.Fatalf("decoding %s: %v\n%s", o.key(), err, o.Doc)
	}
	return ss
}

func TestRedoRendersInThePlatformNamespace(t *testing.T) {
	cfg := deploymentConfig("prod")
	cfg.RedoStorageClass = "regional-pd"
	objs := renderedObjects(t, generateDeployment(t, cfg))

	const ns = "tac-prod"
	ss := decodeStatefulSet(t, findObject(t, objs, "StatefulSet", ns, "redo"))
	findObject(t, objs, "ServiceAccount", ns, "redo")
	findObject(t, objs, "Service", ns, "redo-0")

	if ss.Spec.Replicas == nil || *ss.Spec.Replicas != 1 {
		t.Errorf("redo replicas = %v, want 1: redo is one instance, never replicated", ss.Spec.Replicas)
	}
	// Its volume is the regional disk a local ack is durable on: a claim on the
	// StorageClass the cluster pipeline provides, mounted at redo's data_dir.
	if n := len(ss.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("redo volumeClaimTemplates = %d, want 1", n)
	}
	if sc := ss.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "regional-pd" {
		t.Errorf("redo storageClassName = %v, want regional-pd", sc)
	}
	var mounted bool
	for _, m := range ss.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "data" {
			mounted = true
			if m.MountPath != RedoDataDir {
				t.Errorf("redo data mounted at %s, want %s (redo.Config.data_dir)", m.MountPath, RedoDataDir)
			}
		}
	}
	if !mounted {
		t.Error("redo mounts no data volume")
	}

	for _, o := range objs {
		if strings.HasPrefix(o.Name, "redo") && o.Namespace != ns {
			t.Errorf("%s rendered outside the platform namespace %s", o.key(), ns)
		}
	}
}

// Unset, the claim names no class and the cluster's default applies.
func TestRedoWithoutStorageClassTakesTheClusterDefault(t *testing.T) {
	objs := renderedObjects(t, generateDeployment(t, deploymentConfig("prod")))
	ss := decodeStatefulSet(t, findObject(t, objs, "StatefulSet", "tac-prod", "redo"))
	if sc := ss.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc != nil {
		t.Errorf("redo storageClassName = %q, want none", *sc)
	}
}

// In the one-namespace layout redo is in the one namespace, like every service.
func TestRedoInTheOneNamespaceLayout(t *testing.T) {
	cfg := deploymentConfig("")
	cfg.Namespace = "schematic"
	objs := renderedObjects(t, generateDeployment(t, cfg))
	findObject(t, objs, "StatefulSet", "schematic", "redo")
}

// redo exists for Stratum, which every fleet renders (people always has one):
// a fleet with no tenant of its own still renders redo.
func TestRedoWithOnlyThePeopleStratum(t *testing.T) {
	cfg := deploymentConfig("prod")
	cfg.Tenants, cfg.TenantIds = nil, nil
	cfg.Topology.AppServices = nil
	objs := renderedObjects(t, generateDeployment(t, cfg))
	findObject(t, objs, "StatefulSet", "tac-prod-00000001", "stratum-people")
	findObject(t, objs, "StatefulSet", "tac-prod", "redo")
}
