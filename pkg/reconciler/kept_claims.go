package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tacplus-dev/operator/pkg/deployment"
	protoOperator "github.com/tacplus-dev/operator/tacplus/types/services/operator"
)

// keptClaimsMessage is the warning reportKeptClaims logs, one per StatefulSet.
const keptClaimsMessage = "kept the volume claims of a StatefulSet the chart no longer runs; " +
	"they hold its data and are bound again if it comes back: delete them to drop it"

// reportKeptClaims names the PersistentVolumeClaims a StatefulSet's
// volumeClaimTemplates made that no pod of it mounts any more: those of a
// StatefulSet the chart no longer renders (a component scaled to 0, or the
// shared stratum, whose writers had volumes, retired for per-tenant Stratum in
// memory, #2) and those past its replicas (a scale-down). The operator keeps them, on purpose (#54): a count of 0 is also
// what a missing topology field or a services filter gives, and deleting a
// record store's volumes on that would lose its data for good. It never
// deletes one; it logs a warning per StatefulSet with the claims and the
// command that removes them, once per change in what it keeps. A StatefulSet
// that comes back binds its kept claims again, with whatever they hold.
//
// Nothing here fails the reconcile.
func (r *Reconciler) reportKeptClaims(ctx context.Context, cfg *protoOperator.Config) {
	reader := r.apiReader
	if reader == nil {
		reader = r.client
	}
	seen := map[string]string{}
	for _, ns := range claimNamespaces(cfg) {
		kept, err := keptClaims(ctx, reader, ns)
		if err != nil {
			r.log.Warn("cannot report kept volume claims", zap.String("namespace", ns), zap.Error(err))
			// Unknown is not gone: keep what was reported until it can be read.
			for key, v := range r.keptReported {
				if strings.HasPrefix(key, ns+"/") {
					seen[key] = v
				}
			}
			continue
		}
		for _, st := range sortedKeys(kept) {
			names := kept[st]
			key, v := ns+"/"+st, strings.Join(names, " ")
			seen[key] = v
			if r.keptReported[key] == v {
				continue
			}
			r.log.Warn(keptClaimsMessage,
				zap.String("namespace", ns),
				zap.String("statefulset", st),
				zap.Strings("claims", names),
				zap.String("remove", fmt.Sprintf("kubectl delete pvc -n %s %s", ns, v)))
		}
	}
	r.keptReported = seen
}

// claimNamespaces are the namespaces the chart runs StatefulSets in: the
// platform namespace and, with a deployment, every tenant namespace.
func claimNamespaces(cfg *protoOperator.Config) []string {
	nss := []string{deployment.PlatformNamespace(cfg)}
	for tenant := range deployment.TenantTids(cfg) {
		nss = append(nss, deployment.TenantNamespaceOf(cfg, tenant))
	}
	slices.Sort(nss)
	return slices.Compact(nss)
}

// keptClaims returns, by StatefulSet name, the sorted claims in ns that a
// StatefulSet's volumeClaimTemplate made (named <template>-<set>-<ordinal> and
// carrying the set's selector label app=<set>, as the chart's selectors are)
// and that no pod of it mounts: the set is gone, or the ordinal is past its
// replicas.
func keptClaims(ctx context.Context, reader client.Reader, ns string) (map[string][]string, error) {
	var sets appsv1.StatefulSetList
	if err := reader.List(ctx, &sets, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	replicas := map[string]int{}
	for _, st := range sets.Items {
		n := 1
		if st.Spec.Replicas != nil {
			n = int(*st.Spec.Replicas)
		}
		replicas[st.Name] = n
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &pvcs, client.InNamespace(ns), client.HasLabels{"app"}); err != nil {
		return nil, fmt.Errorf("listing volume claims: %w", err)
	}
	kept := map[string][]string{}
	for _, pvc := range pvcs.Items {
		st := pvc.Labels["app"]
		i := strings.LastIndex(pvc.Name, "-")
		if i < 0 || !strings.HasSuffix(pvc.Name[:i], "-"+st) {
			continue
		}
		ordinal, err := strconv.Atoi(pvc.Name[i+1:])
		if err != nil || ordinal < 0 {
			continue
		}
		if n, live := replicas[st]; live && ordinal < n {
			continue
		}
		kept[st] = append(kept[st], pvc.Name)
	}
	for st := range kept {
		slices.Sort(kept[st])
	}
	return kept, nil
}
