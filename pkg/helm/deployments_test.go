package helm

import (
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/releaseutil"
	networkingv1 "k8s.io/api/networking/v1"
	k8syaml "sigs.k8s.io/yaml"

	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// renderedObject is one manifest of a rendered chart, with where it lands.
type renderedObject struct {
	Kind, Namespace, Name, Doc string
}

func (o renderedObject) key() string { return o.Kind + "/" + o.Namespace + "/" + o.Name }

// renderedObjects renders charts with helm (client-only dry run, as
// renderedManifests does) and returns every object with its namespace.
func renderedObjects(t *testing.T, charts *Charts) []renderedObject {
	t.Helper()
	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{Name: name, Data: data})
	}
	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		t.Fatalf("loading chart: %v", err)
	}
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = "schematic"
	install.Namespace = "default"
	install.DryRun = true
	install.ClientOnly = true
	rel, err := install.Run(ch, map[string]interface{}{})
	if err != nil {
		t.Fatalf("helm dry-run render: %v", err)
	}
	var out []renderedObject
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var head struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatalf("decoding rendered manifest: %v\n%s", err, doc)
		}
		out = append(out, renderedObject{head.Kind, head.Metadata.Namespace, head.Metadata.Name, doc})
	}
	return out
}

func findObject(t *testing.T, objs []renderedObject, kind, ns, name string) renderedObject {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && o.Namespace == ns && o.Name == name {
			return o
		}
	}
	var have []string
	for _, o := range objs {
		have = append(have, o.key())
	}
	t.Fatalf("no %s %s/%s rendered; have %v", kind, ns, name, have)
	return renderedObject{}
}

// deploymentConfig is a deployment with two tenants, a tenant-owned app
// service and the services that carry peer lists.
func deploymentConfig(name string) *protoOperator.Config {
	return &protoOperator.Config{
		Deployment: name,
		Port:       443,
		Tenants:    []string{"acme", "beta"},
		TenantIds:  map[string]string{"acme": "a1b2c3d4", "beta": "b1c2d3e4"},
		Topology: &protoOperator.Topology{
			KernelNodes:    3,
			EventbusNodes:  1,
			GatewayNodes:   2,
			OauthNodes:     1,
			OpaNodes:       1,
			StratumNodes:   2,
			BlobstoreNodes: 2,
			AppServices:    map[string]int32{"docmanager": 1, "widgeter": 1},
		},
		OptionalServices: &protoOperator.OptionalServices{Blobstore: true},
	}
}

func generateDeployment(t *testing.T, cfg *protoOperator.Config) *Charts {
	t.Helper()
	charts, err := New("test", "1.2", nil, map[string]bool{"docmanager": true}, nil).Generate(cfg, 0, false)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return charts
}

func TestDeploymentRendersPlatformAndTenantNamespaces(t *testing.T) {
	objs := renderedObjects(t, generateDeployment(t, deploymentConfig("prod")))
	// The platform and the shared app service in tac-prod.
	findObject(t, objs, "StatefulSet", "tac-prod", "kernel")
	findObject(t, objs, "Deployment", "tac-prod", "gateway0")
	findObject(t, objs, "Deployment", "tac-prod", "widgeter0")
	findObject(t, objs, "Service", "tac-prod", "gateway-edge")
	// Each tenant's instance, with everything it owns, in its own namespace.
	for tenant, ns := range map[string]string{"acme": "tac-prod-a1b2c3d4", "beta": "tac-prod-b1c2d3e4"} {
		name := "docmanager-" + tenant
		findObject(t, objs, "Deployment", ns, name+"0")
		findObject(t, objs, "ServiceAccount", ns, name)
		findObject(t, objs, "Service", ns, name+"0")
		findObject(t, objs, "ConfigMap", ns, name+"0-config")
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Name, "docmanager") && !strings.HasPrefix(o.Namespace, "tac-prod-") {
			t.Errorf("tenant instance %s rendered into %q, not its tenant namespace", o.key(), o.Namespace)
		}
		if o.Namespace == "" {
			t.Errorf("%s has no namespace", o.key())
		}
	}
}

// Nothing cluster-scoped, in either layout: the kernel's TokenReview right is
// the cluster pipeline's system:auth-delegator binding now.
func TestChartsRenderNothingClusterScoped(t *testing.T) {
	for name, charts := range map[string]*Charts{
		"deployment": generateDeployment(t, deploymentConfig("prod")),
		"one namespace": generateTestChart(t, &protoOperator.Config{
			Namespace: "schematic",
			Topology:  &protoOperator.Topology{KernelNodes: 1, GatewayNodes: 1},
		}),
	} {
		for _, o := range renderedObjects(t, charts) {
			if clusterScopedKinds[o.Kind] {
				t.Errorf("%s: rendered cluster-scoped %s", name, o.key())
			}
		}
		for path := range charts.Files {
			if strings.Contains(path, "tokenreview") {
				t.Errorf("%s: rendered %s", name, path)
			}
		}
	}
}

func TestClusterScopedObjectFailsGeneration(t *testing.T) {
	charts := &Charts{Files: map[string][]byte{
		"templates/ok.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"),
		"templates/bad.yaml": []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: y\n"),
	}}
	err := checkNoClusterScopedObjects(charts)
	if err == nil || !strings.Contains(err.Error(), "ClusterRoleBinding") || !strings.Contains(err.Error(), "templates/bad.yaml") {
		t.Fatalf("checkNoClusterScopedObjects = %v, want an error naming the ClusterRoleBinding in templates/bad.yaml", err)
	}
	if err := checkNoClusterScopedObjects(&Charts{Files: map[string][]byte{"templates/ok.yaml": charts.Files["templates/ok.yaml"]}}); err != nil {
		t.Fatalf("checkNoClusterScopedObjects(namespaced only) = %v", err)
	}
}

func TestInvalidDeploymentFailsGeneration(t *testing.T) {
	for name, cfg := range map[string]*protoOperator.Config{
		"bad name":           {Deployment: "Prod", Topology: &protoOperator.Topology{KernelNodes: 1}},
		"tenant without tid": {Deployment: "prod", Tenants: []string{"acme"}, Topology: &protoOperator.Topology{KernelNodes: 1, GatewayNodes: 1}},
		"no gateway_nodes":   {Deployment: "prod", Topology: &protoOperator.Topology{KernelNodes: 1}},
	} {
		if _, err := New("test", "1.2", nil, nil, nil).Generate(cfg, 0, false); err == nil {
			t.Errorf("%s: Generate() succeeded, want an error", name)
		}
	}
}

// Peer lists a pod reads from env name full service names: a tenant namespace
// resolves a short name in its own namespace, never in tac-<deployment>.
func TestPeerListsUseFullServiceNames(t *testing.T) {
	charts := generateDeployment(t, deploymentConfig("prod"))
	stratum := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	if !strings.Contains(stratum, "stratum-0.stratum.tac-prod.svc.cluster.local:5001,stratum-1.stratum.tac-prod.svc.cluster.local:5001") {
		t.Errorf("STRATUM_SEED_MEMBERS does not name full service names:\n%s", stratum)
	}
	blob := chartFile(t, charts, "templates/blobstore/statefulset.yaml")
	if !strings.Contains(blob, "blobstore-0.blobstore.tac-prod.svc.cluster.local:8081") {
		t.Errorf("BLOBSTORE_PEERS does not name full service names:\n%s", blob)
	}
	if got := StratumSeedMembers("tac-prod", 1, 0); len(got) != 1 || got[0] != "stratum-0.stratum.tac-prod.svc.cluster.local:5001" {
		t.Errorf("StratumSeedMembers = %v", got)
	}
}

func decodePolicy(t *testing.T, o renderedObject) networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	if err := k8syaml.Unmarshal([]byte(o.Doc), &np); err != nil {
		t.Fatalf("decoding %s: %v\n%s", o.key(), err, o.Doc)
	}
	return np
}

// Two deployments on one cluster, with the same tenants and even the same tids,
// render no object the other also renders.
func TestTwoDeploymentsShareNoObject(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"prod", "dev"} {
		cfg := deploymentConfig(name)
		cfg.OptionalServices.Echo = true
		cfg.Topology.EchoNodes = 1
		for _, o := range renderedObjects(t, generateDeployment(t, cfg)) {
			if clusterScopedKinds[o.Kind] {
				t.Errorf("%s renders cluster-scoped %s", name, o.key())
			}
			if other, dup := seen[o.key()]; dup && other != name {
				t.Errorf("%s and %s both render %s", other, name, o.key())
			}
			seen[o.key()] = name
		}
	}
}

func TestCoopPigeonsRunInTenantNamespaces(t *testing.T) {
	cfg := deploymentConfig("prod")
	cfg.Topology.AppServices["coop"] = 1
	objs := renderedObjects(t, generateDeployment(t, cfg))
	for _, ns := range []string{"tac-prod-a1b2c3d4", "tac-prod-b1c2d3e4"} {
		role := findObject(t, objs, "Role", ns, "coop-pigeons")
		if !strings.Contains(role.Doc, `resources: ["jobs"]`) {
			t.Errorf("%s: pigeons Role grants no jobs:\n%s", ns, role.Doc)
		}
		binding := findObject(t, objs, "RoleBinding", ns, "coop-pigeons")
		if !strings.HasSuffix(strings.TrimSpace(binding.Doc), "name: coop\n  namespace: tac-prod") {
			t.Errorf("%s: RoleBinding does not bind the coop's service account in tac-prod:\n%q", ns, binding.Doc)
		}
	}
	role := findObject(t, objs, "Role", "tac-prod-a1b2c3d4", "coop-pigeons")
	if strings.Contains(role.Doc, `"rolebindings"`) || strings.Contains(role.Doc, `"roles"`) {
		t.Errorf("coop pigeons Role must grant no RBAC verbs:\n%s", role.Doc)
	}
	dep := findObject(t, objs, "Deployment", "tac-prod", "coop0")
	if !strings.Contains(dep.Doc, "kubernetes-api-token") || !strings.Contains(dep.Doc, "/var/run/secrets/kubernetes.io/serviceaccount") {
		t.Errorf("coop deployment mounts no projected API token:\n%s", dep.Doc)
	}
	for _, o := range objs {
		if o.Name == "widgeter-pigeons" {
			t.Errorf("an app service that is not the coop got pigeons RBAC: %s", o.key())
		}
		if o.Kind == "Namespace" || o.Kind == "ResourceQuota" {
			t.Errorf("rendered %s: tenant namespaces and their quota are deploymentproxy's", o.key())
		}
	}

	// Without a deployment the pigeons would need a namespace of their own,
	// which is cluster-scoped: generation refuses rather than render it.
	legacy := &protoOperator.Config{Namespace: "schematic", Topology: &protoOperator.Topology{KernelNodes: 1, AppServices: map[string]int32{"coop": 1}}}
	if _, err := New("test", "1.2", nil, nil, nil).Generate(legacy, 1, false); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Errorf("coop without a deployment: Generate() = %v, want an error asking for a deployment", err)
	}
}

// Isolation is the bearer token checked at every door, not the network
// (tacplus-dev/roadmap decisions/s2s-never-crosses-tenants-2026-10-31.md,
// addendum 2026-11-07 (2); #24): the charts render no NetworkPolicy but
// kernel's Raft one, the exception for a port with no authentication of its
// own, in either layout.
func TestChartsRenderNoNetworkPolicyButKernelRaft(t *testing.T) {
	for name, c := range map[string]struct {
		charts *Charts
		ns     string
	}{
		"deployment": {generateDeployment(t, deploymentConfig("prod")), "tac-prod"},
		"one namespace": {generateTestChart(t, &protoOperator.Config{
			Namespace: "schematic",
			Topology:  &protoOperator.Topology{KernelNodes: 3, GatewayNodes: 1, StratumNodes: 3, StratumSearchNodes: 1},
		}), "schematic"},
	} {
		var raft bool
		for _, o := range renderedObjects(t, c.charts) {
			if o.Kind != "NetworkPolicy" {
				continue
			}
			if o.Namespace == c.ns && o.Name == "kernel-raft" {
				raft = true
				continue
			}
			t.Errorf("%s: rendered %s; the only NetworkPolicy is kernel's Raft one", name, o.key())
		}
		if !raft {
			t.Errorf("%s: no kernel-raft NetworkPolicy in %s", name, c.ns)
		}
	}
}
