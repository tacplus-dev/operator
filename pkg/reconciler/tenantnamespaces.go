package reconciler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	dpv1 "github.com/tacplus-dev/deploymentproxy/api/v1"
	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tacplus-dev/operator/pkg/deployment"
	"github.com/tacplus-dev/operator/pkg/helm"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// TenantNamespaceReadyTimeout bounds how long one reconcile waits for
// deploymentproxy to put the tenant namespaces in place. Past it the reconcile
// fails and the next tick tries again.
const TenantNamespaceReadyTimeout = 2 * time.Minute

// tenantNamespacePollInterval is how often the wait re-reads the
// TenantNamespace objects.
const tenantNamespacePollInterval = 2 * time.Second

// tenantAnnotation names, on a TenantNamespace the operator made, the tenant
// it is for; the object itself is named by the tenant's tid.
const tenantAnnotation = "tacplus.dev/tenant"

// readyCondition is deploymentproxy's condition on a TenantNamespace. A False
// one whose reason is pendingReasons' is still being worked on; any other
// reason is a refusal that waiting will not change.
const readyCondition = "Ready"

var pendingReasons = map[string]bool{"Pending": true, "Error": true}

// syncTenantNamespaces brings the TenantNamespace objects in cfg's platform
// namespace to the tenants that get a tenant namespace
// (deployment.TenantTids), and waits until deploymentproxy reports each of
// them Ready, so that the helm apply that follows installs into namespaces
// that exist. One is created for a tenant that has none, kept as it is when
// it exists, and, when prune is set, deleted when no tenant has its tid any
// more, which makes deploymentproxy delete the tenant's namespace (decision
// tenant-deletion-2026-10-27). prune is set only when cfg's tenants are known
// to be complete (ServiceConfigs.TenantsComplete). A tenant whose object has another tid is an
// error and nothing is changed: spec.tid is immutable, and deleting the
// object would delete the tenant's namespace.
//
// Without a deployment there are no tenant namespaces, and this does nothing.
func (r *Reconciler) syncTenantNamespaces(ctx context.Context, cfg *protoOperator.Config, prune bool) error {
	if cfg.GetDeployment() == "" {
		return nil
	}
	ns := deployment.PlatformNamespace(cfg)
	desired := deployment.TenantTids(cfg)

	var list dpv1.TenantNamespaceList
	if err := r.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return fmt.Errorf("listing tenant namespaces in %s: %w", ns, err)
	}

	tenantOf := make(map[string]string, len(desired))
	for tenant, tid := range desired {
		tenantOf[tid] = tenant
	}
	existing := map[string]*dpv1.TenantNamespace{}
	var conflicts []error
	for i := range list.Items {
		tn := &list.Items[i]
		existing[tn.Spec.Tid] = tn
		if _, ok := tenantOf[tn.Name]; ok && tn.Spec.Tid != tn.Name {
			conflicts = append(conflicts, fmt.Errorf("TenantNamespace %s/%s has tid %s; the operator names each by its tid", ns, tn.Name, tn.Spec.Tid))
		}
		tenant := tn.Annotations[tenantAnnotation]
		if want, ok := desired[tenant]; ok && tn.Spec.Tid != want {
			conflicts = append(conflicts, fmt.Errorf("tenant %q has tid %s, but its TenantNamespace %s/%s has tid %s, which cannot change",
				tenant, want, ns, tn.Name, tn.Spec.Tid))
		}
	}
	if len(conflicts) > 0 {
		return errors.Join(conflicts...)
	}

	for _, tid := range sortedKeys(tenantOf) {
		if _, ok := existing[tid]; ok {
			continue
		}
		tenant := tenantOf[tid]
		tn := &dpv1.TenantNamespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        tid,
				Namespace:   ns,
				Labels:      map[string]string{helm.ManagedByLabel: helm.ManagedByValue},
				Annotations: map[string]string{tenantAnnotation: tenant},
			},
			Spec: dpv1.TenantNamespaceSpec{Tid: tid},
		}
		if err := r.client.Create(ctx, tn); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating TenantNamespace %s/%s for tenant %q: %w", ns, tid, tenant, err)
		}
		r.log.Info("asked deploymentproxy for a tenant namespace",
			zap.String("tenant", tenant),
			zap.String("tid", tid),
			zap.String("tenant_namespace", deployment.TenantNamespace(cfg.GetDeployment(), tid)),
		)
	}

	for _, tid := range sortedKeys(existing) {
		tn := existing[tid]
		if _, ok := tenantOf[tid]; ok || tn.DeletionTimestamp != nil {
			continue
		}
		if !prune {
			r.log.Info("keeping the TenantNamespace of a tenant not in this pass's records; kernel's records are not read yet",
				zap.String("tenant", tn.Annotations[tenantAnnotation]),
				zap.String("tid", tid),
			)
			continue
		}
		r.log.Warn("deleting the TenantNamespace of a removed tenant; deploymentproxy deletes its namespace",
			zap.String("tenant", tn.Annotations[tenantAnnotation]),
			zap.String("tid", tid),
			zap.String("tenant_namespace", deployment.TenantNamespace(cfg.GetDeployment(), tid)),
		)
		if err := r.client.Delete(ctx, tn); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting TenantNamespace %s/%s: %w", ns, tn.Name, err)
		}
	}

	return r.waitTenantNamespacesReady(ctx, ns, tenantOf)
}

// waitTenantNamespacesReady polls the TenantNamespace of every tid in
// tenantOf until each reports Ready=True. A refusal fails at once; a
// TenantNamespace still pending when the wait is over fails with its last
// condition.
func (r *Reconciler) waitTenantNamespacesReady(ctx context.Context, ns string, tenantOf map[string]string) error {
	timeout := r.tenantNamespaceTimeout
	if timeout == 0 {
		timeout = TenantNamespaceReadyTimeout
	}
	poll := r.tenantNamespacePoll
	if poll == 0 {
		poll = tenantNamespacePollInterval
	}

	var notReady error
	err := wait.PollUntilContextTimeout(ctx, poll, timeout, true, func(ctx context.Context) (bool, error) {
		var pending, refused []error
		for _, tid := range sortedKeys(tenantOf) {
			tenant := tenantOf[tid]
			var tn dpv1.TenantNamespace
			if err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: tid}, &tn); err != nil {
				if !apierrors.IsNotFound(err) {
					return false, fmt.Errorf("reading TenantNamespace %s/%s: %w", ns, tid, err)
				}
				// The cache has not seen the create yet.
				pending = append(pending, fmt.Errorf("tenant %q (tid %s): TenantNamespace %s/%s not found", tenant, tid, ns, tid))
				continue
			}
			cond := apimeta.FindStatusCondition(tn.Status.Conditions, readyCondition)
			switch {
			case cond == nil:
				pending = append(pending, fmt.Errorf("tenant %q (tid %s): deploymentproxy has not answered", tenant, tid))
			case cond.Status == metav1.ConditionTrue:
			case pendingReasons[cond.Reason]:
				pending = append(pending, fmt.Errorf("tenant %q (tid %s): tenant namespace not ready: %s: %s", tenant, tid, cond.Reason, cond.Message))
			default:
				refused = append(refused, fmt.Errorf("tenant %q (tid %s): deploymentproxy refused its tenant namespace: %s: %s", tenant, tid, cond.Reason, cond.Message))
			}
		}
		if len(refused) > 0 {
			return false, errors.Join(refused...)
		}
		notReady = errors.Join(pending...)
		return notReady == nil, nil
	})
	if err == nil {
		return nil
	}
	if wait.Interrupted(err) && notReady != nil && ctx.Err() == nil {
		return fmt.Errorf("waited %v for tenant namespaces: %w", timeout, notReady)
	}
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
