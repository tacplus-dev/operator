package reconciler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	dpv1 "github.com/tacplus-dev/deploymentproxy/api/v1"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/tacplus-dev/operator/pkg/deployment"
	"github.com/tacplus-dev/operator/pkg/helm"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// tenantNamespaceTestConfig is deployment prod with the one tenant acme.
func tenantNamespaceTestConfig() *protoOperator.Config {
	return &protoOperator.Config{
		Deployment: "prod",
		Tenants:    []string{"acme"},
		TenantIds:  map[string]string{"acme": "a1b2c3d4"},
	}
}

func tenantNamespaceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dpv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// existingTenantNamespace is a TenantNamespace already in tac-prod, Ready.
func existingTenantNamespace(tenant, tid string) *dpv1.TenantNamespace {
	return &dpv1.TenantNamespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tid,
			Namespace:   "tac-prod",
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "schematic-operator"},
			Annotations: map[string]string{tenantAnnotation: tenant},
		},
		Spec: dpv1.TenantNamespaceSpec{Tid: tid},
		Status: dpv1.TenantNamespaceStatus{
			Namespace:  deployment.TenantNamespace("prod", tid),
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Now()}},
		},
	}
}

// tenantNamespaceTestClient is a fake API server holding objs. When answer is
// set it plays deploymentproxy: the TenantNamespace created for acme gets
// answer's Ready condition at once, every other one Ready=True. created
// collects the names created.
func tenantNamespaceTestClient(t *testing.T, answer *metav1.Condition, created *[]string, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(tenantNamespaceTestScheme(t)).
		WithStatusSubresource(&dpv1.TenantNamespace{}).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				tn, ok := obj.(*dpv1.TenantNamespace)
				if !ok {
					return nil
				}
				*created = append(*created, tn.Name)
				if answer == nil {
					return nil
				}
				cond := *answer
				if tn.Spec.Tid != "a1b2c3d4" {
					cond = *readyTrue
				}
				cond.LastTransitionTime = metav1.Now()
				if cond.Status == metav1.ConditionTrue {
					tn.Status.Namespace = deployment.TenantNamespace("prod", tn.Spec.Tid)
				}
				apimeta.SetStatusCondition(&tn.Status.Conditions, cond)
				return c.Status().Update(ctx, tn)
			},
		}).
		Build()
}

func tenantNamespaceTestReconciler(cl client.Client, cfg *protoOperator.Config) *Reconciler {
	return &Reconciler{
		cfg:                    cfg,
		client:                 cl,
		log:                    zap.NewNop(),
		tenantNamespaceTimeout: 2 * time.Second,
		tenantNamespacePoll:    10 * time.Millisecond,
	}
}

func listTenantNamespaces(t *testing.T, cl client.Client) map[string]dpv1.TenantNamespace {
	t.Helper()
	var list dpv1.TenantNamespaceList
	if err := cl.List(context.Background(), &list, client.InNamespace("tac-prod")); err != nil {
		t.Fatalf("listing tenant namespaces: %v", err)
	}
	out := map[string]dpv1.TenantNamespace{}
	for _, tn := range list.Items {
		out[tn.Name] = tn
	}
	return out
}

var readyTrue = &metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", Message: "namespace in place"}

// TestSyncTenantNamespacesCreates: every tenant that gets a tenant namespace,
// the reserved people and restore included, gets a TenantNamespace named by
// its tid in the platform namespace, labelled as the operator's and annotated
// with the tenant's name.
func TestSyncTenantNamespacesCreates(t *testing.T) {
	var created []string
	cl := tenantNamespaceTestClient(t, readyTrue, &created)
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	if err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}

	got := listTenantNamespaces(t, cl)
	for tenant, tid := range map[string]string{"acme": "a1b2c3d4", "people": "00000001", "restore": "00000002"} {
		tn, ok := got[tid]
		if !ok {
			t.Errorf("no TenantNamespace %s for tenant %s", tid, tenant)
			continue
		}
		if tn.Spec.Tid != tid {
			t.Errorf("TenantNamespace %s: spec.tid = %q, want %q", tid, tn.Spec.Tid, tid)
		}
		if tn.Labels["app.kubernetes.io/managed-by"] != "schematic-operator" {
			t.Errorf("TenantNamespace %s: labels = %v, want managed-by schematic-operator", tid, tn.Labels)
		}
		if tn.Annotations["tacplus.dev/tenant"] != tenant {
			t.Errorf("TenantNamespace %s: annotations = %v, want tacplus.dev/tenant %s", tid, tn.Annotations, tenant)
		}
	}
	if len(got) != 3 {
		t.Errorf("TenantNamespaces = %v, want exactly the 3 tenants'", got)
	}
}

// TestSyncTenantNamespacesKeepsExisting: one already there is left alone, not
// recreated or rewritten.
func TestSyncTenantNamespacesKeepsExisting(t *testing.T) {
	existing := existingTenantNamespace("acme", "a1b2c3d4")
	existing.Annotations["kept"] = "yes"
	var created []string
	cl := tenantNamespaceTestClient(t, readyTrue, &created, existing,
		existingTenantNamespace("people", "00000001"), existingTenantNamespace("restore", "00000002"))
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	if err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("created %v, want nothing: every TenantNamespace exists", created)
	}
	if got := listTenantNamespaces(t, cl)["a1b2c3d4"]; got.Annotations["kept"] != "yes" {
		t.Errorf("existing TenantNamespace rewritten: annotations = %v", got.Annotations)
	}
}

// TestSyncTenantNamespacesDeletesRemovedTenants: a TenantNamespace whose tid
// no tenant has any more is deleted, so deploymentproxy deletes the tenant's
// namespace (decision tenant-deletion-2026-10-27).
func TestSyncTenantNamespacesDeletesRemovedTenants(t *testing.T) {
	var created []string
	cl := tenantNamespaceTestClient(t, readyTrue, &created, existingTenantNamespace("gone", "z9y8x7w6"))
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	if err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}
	got := listTenantNamespaces(t, cl)
	if _, ok := got["z9y8x7w6"]; ok {
		t.Error("TenantNamespace z9y8x7w6 of a removed tenant was not deleted")
	}
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"00000001", "00000002", "a1b2c3d4"}; !slices.Equal(names, want) {
		t.Errorf("TenantNamespaces = %v, want %v", names, want)
	}
}

// TestSyncTenantNamespacesKeepsUnreadTenants: until kernel's records are
// read, a TenantNamespace of a tenant missing from this pass may be a record
// tenant's, so none is deleted.
func TestSyncTenantNamespacesKeepsUnreadTenants(t *testing.T) {
	var created []string
	cl := tenantNamespaceTestClient(t, readyTrue, &created, existingTenantNamespace("globex", "g1h2i3j4"))
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	if err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), false); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}
	got := listTenantNamespaces(t, cl)
	if _, ok := got["g1h2i3j4"]; !ok {
		t.Error("TenantNamespace g1h2i3j4 deleted before kernel's records were read")
	}
	if len(got) != 4 {
		t.Errorf("%d TenantNamespaces, want the 3 tenants' and the kept one", len(got))
	}
}

// TestSyncTenantNamespacesRefusesChangedTid: spec.tid is immutable, so a
// tenant whose TenantNamespace has another tid is an error, and nothing is
// created or deleted: deleting it would delete the tenant's namespace.
func TestSyncTenantNamespacesRefusesChangedTid(t *testing.T) {
	var created []string
	cl := tenantNamespaceTestClient(t, readyTrue, &created, existingTenantNamespace("acme", "b0000000"))
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true)
	if err == nil || !strings.Contains(err.Error(), `"acme"`) || !strings.Contains(err.Error(), "b0000000") || !strings.Contains(err.Error(), "a1b2c3d4") {
		t.Fatalf("syncTenantNamespaces = %v, want an error naming acme and both tids", err)
	}
	if len(created) != 0 {
		t.Errorf("created %v, want nothing", created)
	}
	if _, ok := listTenantNamespaces(t, cl)["b0000000"]; !ok {
		t.Error("TenantNamespace b0000000 deleted")
	}
}

// TestSyncTenantNamespacesWaitsForReady: the sync returns only once every
// TenantNamespace reports Ready=True, here a while after it is created.
func TestSyncTenantNamespacesWaitsForReady(t *testing.T) {
	var created []string
	pending := &metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending", Message: "namespace terminating"}
	cl := tenantNamespaceTestClient(t, pending, &created)
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())

	go func() {
		time.Sleep(100 * time.Millisecond)
		var list dpv1.TenantNamespaceList
		if err := cl.List(context.Background(), &list, client.InNamespace("tac-prod")); err != nil {
			t.Errorf("listing tenant namespaces: %v", err)
			return
		}
		for _, tn := range list.Items {
			apimeta.SetStatusCondition(&tn.Status.Conditions, *readyTrue)
			tn.Status.Namespace = deployment.TenantNamespace("prod", tn.Spec.Tid)
			if err := cl.Status().Update(context.Background(), &tn); err != nil {
				t.Errorf("marking %s ready: %v", tn.Name, err)
			}
		}
	}()

	start := time.Now()
	if err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, before the TenantNamespaces were Ready", elapsed)
	}
}

// TestSyncTenantNamespacesReportsRefusal: deploymentproxy refusing a
// TenantNamespace fails the sync at once, naming the tenant, its tid and the
// condition's reason and message.
func TestSyncTenantNamespacesReportsRefusal(t *testing.T) {
	var created []string
	refused := &metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NamespaceConflict", Message: "tac-prod-a1b2c3d4 exists and is not ours"}
	cl := tenantNamespaceTestClient(t, refused, &created)
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())
	r.tenantNamespaceTimeout = time.Minute

	start := time.Now()
	err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true)
	if err == nil {
		t.Fatal("syncTenantNamespaces succeeded, want the refusal")
	}
	for _, want := range []string{`"acme"`, "a1b2c3d4", "NamespaceConflict", "is not ours"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("refusal reported after %v, want at once", elapsed)
	}
}

// TestSyncTenantNamespacesTimesOut: a TenantNamespace that never turns Ready
// fails the sync once the wait is over, with its last condition.
func TestSyncTenantNamespacesTimesOut(t *testing.T) {
	var created []string
	pending := &metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending", Message: "namespace terminating"}
	cl := tenantNamespaceTestClient(t, pending, &created)
	r := tenantNamespaceTestReconciler(cl, tenantNamespaceTestConfig())
	r.tenantNamespaceTimeout = 100 * time.Millisecond

	err := r.syncTenantNamespaces(context.Background(), tenantNamespaceTestConfig(), true)
	if err == nil {
		t.Fatal("syncTenantNamespaces succeeded, want a timeout")
	}
	for _, want := range []string{"a1b2c3d4", "Pending", "namespace terminating"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestSyncTenantNamespacesWithoutDeployment: a fleet without a deployment has
// no tenant namespaces, and need not have the CRD: nothing is read or written.
func TestSyncTenantNamespacesWithoutDeployment(t *testing.T) {
	called := false
	cl := fake.NewClientBuilder().WithScheme(tenantNamespaceTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			called = true
			return errors.New("unexpected get")
		},
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
			called = true
			return errors.New("unexpected list")
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			called = true
			return errors.New("unexpected create")
		},
	}).Build()
	cfg := &protoOperator.Config{Namespace: "schematic", Tenants: []string{"acme"}}
	r := tenantNamespaceTestReconciler(cl, cfg)

	if err := r.syncTenantNamespaces(context.Background(), cfg, true); err != nil {
		t.Fatalf("syncTenantNamespaces: %v", err)
	}
	if called {
		t.Error("syncTenantNamespaces called the API server without a deployment")
	}
}

// TestReconcileInDeploymentModeListsNoNodes: a deployment's operator has no
// cluster-wide rights, so reconcile must not list nodes; it gets as far as
// the helm apply with the TenantNamespaces in place.
func TestReconcileInDeploymentModeListsNoNodes(t *testing.T) {
	listedNodes := false
	cl := fake.NewClientBuilder().
		WithScheme(tenantNamespaceTestScheme(t)).
		WithStatusSubresource(&dpv1.TenantNamespace{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NodeList); ok {
					listedNodes = true
					return errors.New("nodes is forbidden")
				}
				return c.List(ctx, list, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				tn, ok := obj.(*dpv1.TenantNamespace)
				if !ok {
					return nil
				}
				cond := *readyTrue
				cond.LastTransitionTime = metav1.Now()
				apimeta.SetStatusCondition(&tn.Status.Conditions, cond)
				return c.Status().Update(ctx, tn)
			},
		}).
		Build()
	cfg := tenantNamespaceTestConfig()
	cfg.Topology = &protoOperator.Topology{KernelNodes: 1, GatewayNodes: 2}
	r := tenantNamespaceTestReconciler(cl, cfg)
	r.generator = helm.New("1-2", "1.2", nil, nil, nil)
	// Nothing listens here: the helm apply fails, after everything before it.
	r.restCfg = &rest.Config{Host: "http://127.0.0.1:1"}

	err := r.reconcile(context.Background())
	if listedNodes {
		t.Error("reconcile listed nodes in deployment mode")
	}
	if err == nil || !strings.Contains(err.Error(), "applying charts") {
		t.Fatalf("reconcile = %v, want it to fail only at the helm apply", err)
	}
	if got := len(listTenantNamespaces(t, cl)); got != 3 {
		t.Errorf("%d TenantNamespaces before the helm apply, want 3", got)
	}
}
