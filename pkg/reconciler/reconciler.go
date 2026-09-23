// Package reconciler implements the in-cluster operator reconciliation loop.
//
// When mode = IN_CLUSTER, the reconciler:
//  1. Watches the Kubernetes node count (schedulable worker nodes).
//  2. On node count change or periodic tick, regenerates the schematic Helm chart.
//  3. Applies the chart to the cluster via Helm install/upgrade.
//
// Image selection uses the new versioning scheme:
//
//	v{MAJOR}.{MINOR}.{PROTO_PATCH}-{BRANCH}.{LOCAL_PATCH}
//
// The upgrade.Checker resolves the latest LOCAL_PATCH per service and signals
// this reconciler when newer versions are available. The reconciler performs a
// self-upgrade if the operator itself has a newer LOCAL_PATCH, then reconciles
// all services.
//
// The reconciler uses controller-runtime for lifecycle management and
// leader election, ensuring only one operator replica is active at a time.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmrelease "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/releaseutil"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	protoServicesConfig "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/generated/services_config"
	protoKernel "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kernel"
	protoOpa "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/opa"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/upgrade"
)

// ReconcileInterval is how often the reconciler re-checks cluster state
// even when no watch events fire.
const ReconcileInterval = 30 * time.Second

// StalePendingReleaseAge is the minimum age before the reconciler treats a
// pending Helm release as abandoned by a crashed or replaced operator.
const StalePendingReleaseAge = 5 * time.Minute

const releaseName = "schematic"

// DefaultNamespace is the namespace the fleet is deployed into when
// operator.yaml does not set one.
const DefaultNamespace = "schematic"

// ServiceConfigs is one reconcile's worth of generated configuration.
type ServiceConfigs struct {
	// Values are the per-service Helm values (map of service name →
	// {"config": yaml-string}). Nil means "no overrides — use chart defaults".
	Values map[string]interface{}

	// Opa is the policy-bearing OPA config kernel serves as opa's config-state
	// root. It is not the same thing as Values["opa"], which is the plain
	// chassis bootstrap file the opa pod mounts and carries no policies.
	// Nil when the config directory declares no opa config.
	Opa *protoOpa.Config
}

// ServiceConfigProvider returns the generated configuration on each reconcile.
// It is called once per reconcile loop iteration. Returning nil, nil means
// "no overrides — use chart defaults".
type ServiceConfigProvider func() (*ServiceConfigs, error)

// OpaConfigPusher pushes the OPA config to the kernel, which stores it as
// opa's config-state root so a running cluster picks up a policy change
// without a redeploy. The generated kernel MintClient satisfies it.
type OpaConfigPusher interface {
	UpdateOpaConfig(ctx context.Context, req *protoKernel.UpdateOpaConfigRequest, opts ...grpc.CallOption) error
}

// Reconciler manages the schematic Helm release inside the cluster.
type Reconciler struct {
	cfg            *protoOperator.Config
	configProvider ServiceConfigProvider
	checker        *upgrade.Checker
	generator      *helm.Generator
	client         client.Client
	log            *zap.Logger
	restCfg        *rest.Config
	mapper         apimeta.RESTMapper
	operatorNS     string
	series         string // image series from operator binary, e.g. "1-2"
	versionLock    string // MAJOR.MINOR from operator binary, e.g. "1.2"

	// opaPusher is nil until the operator's kernel identity is available, and
	// stays nil forever when the operator has none. Guarded by opaMu because
	// it is set from the ServiceManager bootstrap goroutine.
	opaMu       sync.Mutex
	opaPusher   OpaConfigPusher
	lastOpaPush *protoOpa.Config
}

// New creates a Reconciler. Call Run to start the reconciliation loop.
// configProvider is called on each reconcile to obtain per-service Helm values;
// pass nil if service configs are not available (charts will deploy with empty configs).
// operatorNS is the namespace where the operator pod runs (used for reading
// the gitlab-registry secret when auto_upgrade is enabled).
// checker resolves per-service image tags; the generator uses it as a TagResolver.
// tenantOwned names the services registry.json marks tenant_owned, and
// pinnedTenant the single tenant each pinned one of those is pinned to; the
// caller reads both from the embedded registry, which this package cannot
// reach.
func New(cfg *protoOperator.Config, configProvider ServiceConfigProvider, checker *upgrade.Checker, mgr manager.Manager, log *zap.Logger, operatorNS, series, versionLock string, tenantOwned map[string]bool, pinnedTenant map[string]string) *Reconciler {
	return &Reconciler{
		cfg:            cfg,
		configProvider: configProvider,
		checker:        checker,
		generator:      helm.New(series, versionLock, checker, tenantOwned, pinnedTenant),
		client:         mgr.GetClient(),
		log:            log,
		restCfg:        mgr.GetConfig(),
		mapper:         mgr.GetRESTMapper(),
		operatorNS:     operatorNS,
		series:         series,
		versionLock:    versionLock,
	}
}

// Start implements manager.Runnable so the reconciler can be registered with
// the controller-runtime manager via mgr.Add(). The manager calls Start only
// after the k8s cache is ready, guaranteeing that the initial Resolve() can
// read secrets from the API server.
func (r *Reconciler) Start(ctx context.Context) error {
	return r.Run(ctx)
}

// Run starts the reconciliation loop. It blocks until ctx is cancelled.
// The checker resolves image tags and polls for upgrades.
func (r *Reconciler) Run(ctx context.Context) error {
	r.log.Info("reconciler started", zap.String("namespace", r.cfg.GetNamespace()))

	// Resolve initial image tags before the first reconcile.
	if _, err := r.checker.Resolve(ctx); err != nil {
		r.log.Warn("failed to resolve initial image tags, will use latest",
			zap.Error(err),
		)
	} else {
		r.log.Info("resolved initial image tags")
	}

	go r.checker.Run(ctx)
	upgradeCh := r.checker.Notify()

	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()

	// Reconcile immediately on startup.
	if err := r.reconcile(ctx); err != nil {
		r.log.Error("initial reconcile failed", zap.Error(err))
	}

	for {
		select {
		case <-ctx.Done():
			r.log.Info("reconciler stopping")
			return nil
		case <-upgradeCh:
			r.log.Info("auto-upgrade: new versions available")
			// Self-upgrade first. If the operator image changed, k8s will restart
			// this pod; the new pod will reconcile services on startup. Only proceed
			// to service reconcile if the operator is already at the new version.
			selfUpdated, err := r.selfUpgrade(ctx)
			if err != nil {
				r.log.Error("operator self-upgrade failed", zap.Error(err))
			}
			if selfUpdated {
				r.log.Info("operator self-upgrade initiated, waiting for restart")
				continue
			}
			if err := r.reconcile(ctx); err != nil {
				r.log.Error("auto-upgrade reconcile failed", zap.Error(err))
			}
		case <-ticker.C:
			if err := r.reconcile(ctx); err != nil {
				r.log.Error("reconcile failed", zap.Error(err))
			}
		}
	}
}

func (r *Reconciler) namespace() string {
	if ns := r.cfg.GetNamespace(); ns != "" {
		return ns
	}
	return DefaultNamespace
}

// selfUpgrade checks whether the operator's own Deployment is running the
// selfUpgrade checks whether the operator's own Deployment is running the
// desired version. If not, it patches the container image and returns true
// so the caller can skip the service reconcile — k8s will restart the operator
// pod and the new instance will pick up the service upgrade on startup.
// Returns (false, nil) when the operator is already at the desired version.
func (r *Reconciler) selfUpgrade(ctx context.Context) (bool, error) {
	desiredTag := r.checker.OperatorTag()
	if desiredTag == "" {
		// No resolved version yet; skip self-upgrade.
		return false, nil
	}

	var dep appsv1.Deployment
	if err := r.client.Get(ctx, types.NamespacedName{
		Namespace: r.operatorNS,
		Name:      "schematic-operator",
	}, &dep); err != nil {
		return false, fmt.Errorf("getting operator deployment: %w", err)
	}

	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return false, fmt.Errorf("operator deployment has no containers")
	}

	currentImage := dep.Spec.Template.Spec.Containers[0].Image

	// Strip the existing tag to derive the base image URL.
	base := currentImage
	if idx := strings.LastIndex(currentImage, ":"); idx > 0 {
		base = currentImage[:idx]
	}
	desiredImage := base + ":" + desiredTag

	if currentImage == desiredImage {
		return false, nil
	}

	r.log.Info("patching operator image",
		zap.String("from", currentImage),
		zap.String("to", desiredImage),
	)

	p := []byte(fmt.Sprintf(
		`{"spec":{"template":{"spec":{"containers":[{"name":"schematic-operator","image":%q}]}}}}`,
		desiredImage,
	))
	if err := r.client.Patch(ctx, &dep, client.RawPatch(types.StrategicMergePatchType, p)); err != nil {
		return false, fmt.Errorf("patching operator deployment: %w", err)
	}

	return true, nil
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	nodeCount, err := r.schedulableNodeCount(ctx)
	if err != nil {
		return fmt.Errorf("counting nodes: %w", err)
	}
	r.log.Info("reconciling", zap.Int("schedulable_nodes", nodeCount))

	charts, err := r.generator.Generate(r.cfg, nodeCount)
	if err != nil {
		return fmt.Errorf("generating charts: %w", err)
	}

	var svcValues map[string]interface{}
	var opaConfig *protoOpa.Config
	if r.configProvider != nil {
		sc, err := r.configProvider()
		if err != nil {
			return fmt.Errorf("building service configs: %w", err)
		}
		if sc != nil {
			svcValues = sc.Values
			opaConfig = sc.Opa
		}
		r.log.Info("built service configs", zap.Int("services", len(svcValues)))
	}

	if err := r.apply(ctx, charts, svcValues); err != nil {
		return fmt.Errorf("applying charts: %w", err)
	}

	r.pushOpaConfig(ctx, opaConfig)

	// Prune deployments that are no longer in the chart (e.g. after topology scale-down).
	if err := r.pruneStaleDeployments(ctx, nodeCount); err != nil {
		r.log.Warn("failed to prune stale deployments", zap.Error(err))
	}

	r.log.Info("reconcile complete", zap.Int("files", len(charts.Files)))
	return nil
}

// SetOpaConfigPusher installs the kernel client used to push the OPA config.
// It is called from the ServiceManager bootstrap goroutine, which cannot run
// synchronously before the reconciler starts: on a fresh cluster the kernel
// the operator authenticates against does not exist until this reconciler has
// deployed it.
func (r *Reconciler) SetOpaConfigPusher(p OpaConfigPusher) {
	r.opaMu.Lock()
	defer r.opaMu.Unlock()
	r.opaPusher = p
}

// pushOpaConfig sends the OPA config to the kernel so a running cluster picks
// up a policy change without a redeploy.
//
// Pushed on change rather than on every tick: the reconcile loop runs every 30s
// and the config carries the whole rego bundle, so an unconditional push would
// rewrite kernel's config-state root 2880 times a day for nothing. The last
// pushed value is remembered only in memory, so an operator restart pushes once
// on its first reconcile — which is what makes cold start correct.
//
// Failure is logged, not returned: pushing policy to kernel must never fail the
// Helm reconcile that keeps the fleet running. lastOpaPush is only updated on
// success, so a failed push retries on the next reconcile.
func (r *Reconciler) pushOpaConfig(ctx context.Context, cfg *protoOpa.Config) {
	if cfg == nil {
		return
	}

	r.opaMu.Lock()
	pusher := r.opaPusher
	last := r.lastOpaPush
	r.opaMu.Unlock()

	if pusher == nil || proto.Equal(last, cfg) {
		return
	}

	req := &protoKernel.UpdateOpaConfigRequest{
		Config: &protoServicesConfig.Services{OpaConfig: cfg},
	}
	if err := pusher.UpdateOpaConfig(ctx, req); err != nil {
		r.log.Warn("pushing opa config to kernel failed; retrying next reconcile", zap.Error(err))
		return
	}

	r.opaMu.Lock()
	r.lastOpaPush = cfg
	r.opaMu.Unlock()
	r.log.Info("pushed opa config to kernel")
}

// schedulableNodeCount returns the number of Ready, non-tainted worker nodes.
func (r *Reconciler) schedulableNodeCount(ctx context.Context) (int, error) {
	var nodeList corev1.NodeList
	if err := r.client.List(ctx, &nodeList, &client.ListOptions{
		LabelSelector: labels.Everything(),
	}); err != nil {
		return 0, err
	}

	count := 0
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable {
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				count++
				break
			}
		}
	}
	return count, nil
}

// apply installs or upgrades the "schematic" Helm release using the in-memory
// chart files produced by the generator. svcValues overrides values.yaml defaults
// (typically per-service config YAML strings); nil is treated as an empty map.
func (r *Reconciler) apply(ctx context.Context, charts *helm.Charts, svcValues map[string]interface{}) error {
	ns := r.namespace()

	getter := &inClusterGetter{
		restCfg:   r.restCfg,
		mapper:    r.mapper,
		namespace: ns,
	}

	actionCfg := new(action.Configuration)
	if err := actionCfg.Init(getter, ns, "secret", func(format string, v ...interface{}) {
		r.log.Sugar().Debugf(format, v...)
	}); err != nil {
		return fmt.Errorf("initialising helm action config: %w", err)
	}
	actionCfg.Releases.MaxHistory = 10

	buffered := make([]*helmloader.BufferedFile, 0, len(charts.Files))
	for name, data := range charts.Files {
		buffered = append(buffered, &helmloader.BufferedFile{
			Name: name,
			Data: data,
		})
	}

	ch, err := helmloader.LoadFiles(buffered)
	if err != nil {
		return fmt.Errorf("loading helm chart from memory: %w", err)
	}

	latest, histErr := actionCfg.Releases.Last(releaseName)

	if svcValues == nil {
		svcValues = map[string]interface{}{}
	}

	switch {
	case histErr == helmdriver.ErrReleaseNotFound:
		return r.helmInstall(actionCfg, ch, ns, svcValues)
	case histErr != nil:
		return fmt.Errorf("querying helm release history: %w", histErr)
	default:
		if recovered, err := r.recoverStalePendingRelease(actionCfg, latest); err != nil {
			return err
		} else if recovered {
			return nil
		}
		if err := r.removeKernelHotfixDrift(ctx, ns); err != nil {
			return fmt.Errorf("removing kernel hotfix drift: %w", err)
		}
		if err := r.reconcileImmutableStatefulSetFields(ctx, ns, ch, svcValues); err != nil {
			return fmt.Errorf("reconciling immutable statefulset fields: %w", err)
		}
		return r.helmUpgrade(actionCfg, ch, svcValues)
	}
}

func (r *Reconciler) recoverStalePendingRelease(cfg *action.Configuration, latest *helmrelease.Release) (bool, error) {
	if latest == nil || latest.Info == nil || !latest.Info.Status.IsPending() {
		return false, nil
	}

	stale, age := isStalePendingRelease(latest, time.Now())
	if !stale {
		return false, fmt.Errorf("helm release %s revision %d is %s for %s; waiting before recovery",
			latest.Name,
			latest.Version,
			latest.Info.Status,
			age.Truncate(time.Second),
		)
	}

	deployed, err := rollbackTargetRevision(cfg, latest)
	if errors.Is(err, errNoRollbackTarget) {
		return r.uninstallStalePendingRelease(latest, cfg, age)
	}
	if err != nil {
		return false, fmt.Errorf("helm release %s revision %d is stale %s but no rollback revision is available: %w",
			latest.Name,
			latest.Version,
			latest.Info.Status,
			err,
		)
	}

	r.log.Warn("recovering stale pending Helm release",
		zap.String("release", latest.Name),
		zap.Int("pending_revision", latest.Version),
		zap.String("pending_status", latest.Info.Status.String()),
		zap.Duration("age", age.Truncate(time.Second)),
		zap.Int("rollback_revision", deployed.Version),
	)

	rollback := action.NewRollback(cfg)
	rollback.Version = deployed.Version
	rollback.MaxHistory = 10
	rollback.Timeout = 5 * time.Minute
	rollback.CleanupOnFail = true
	if err := rollback.Run(releaseName); err != nil {
		return false, fmt.Errorf("recovering stale pending Helm release by rolling back to revision %d: %w", deployed.Version, err)
	}

	r.log.Info("stale pending Helm release recovered; desired chart will apply on next reconcile",
		zap.String("release", latest.Name),
		zap.Int("recovered_revision", latest.Version),
		zap.Int("rollback_revision", deployed.Version),
	)
	return true, nil
}

// uninstallStalePendingRelease recovers a stale pending release that has no prior
// revision to roll back to — an interrupted first install. There is no earlier state to
// return to, so the stuck release is removed and the next reconcile installs cleanly.
func (r *Reconciler) uninstallStalePendingRelease(latest *helmrelease.Release, cfg *action.Configuration, age time.Duration) (bool, error) {
	r.log.Warn("uninstalling stale pending Helm release with no prior revision",
		zap.String("release", latest.Name),
		zap.Int("pending_revision", latest.Version),
		zap.String("pending_status", latest.Info.Status.String()),
		zap.Duration("age", age.Truncate(time.Second)),
	)

	uninstall := action.NewUninstall(cfg)
	uninstall.Timeout = 5 * time.Minute
	if _, err := uninstall.Run(releaseName); err != nil {
		return false, fmt.Errorf("recovering stale pending Helm release by uninstalling revision %d: %w", latest.Version, err)
	}

	r.log.Info("stale pending Helm release uninstalled; a clean install will run on next reconcile",
		zap.String("release", latest.Name),
		zap.Int("uninstalled_revision", latest.Version),
	)
	return true, nil
}

// errNoRollbackTarget reports that the release has no prior revision to return to, as
// opposed to the history query itself failing. A stale pending release in that state is
// an interrupted first install, which is recovered by uninstalling rather than rolling back.
var errNoRollbackTarget = errors.New("release has no prior revision to roll back to")

// rollbackTargetRevision picks the revision a stale pending release should be rolled
// back to. Normally that is the current deployed revision, but Helm marks the previous
// revision superseded as soon as an upgrade starts, so an upgrade that dies before the
// new revision reaches deployed leaves the release with no deployed revision at all. In
// that case fall back to the most recent superseded or failed revision older than the
// stuck one — a failed predecessor's chart and values are still a real previously
// attempted revision, and Helm's rollback accepts any prior revision regardless of its
// terminal status. With no prior revision at all it returns errNoRollbackTarget.
func rollbackTargetRevision(cfg *action.Configuration, latest *helmrelease.Release) (*helmrelease.Release, error) {
	deployed, err := cfg.Releases.Deployed(releaseName)
	if err == nil {
		return deployed, nil
	}

	history, historyErr := cfg.Releases.History(releaseName)
	if historyErr != nil {
		return nil, fmt.Errorf("no deployed revision (%v); querying release history: %w", err, historyErr)
	}

	var target *helmrelease.Release
	for _, rel := range history {
		if rel.Info == nil || rel.Version >= latest.Version {
			continue
		}
		if rel.Info.Status != helmrelease.StatusSuperseded && rel.Info.Status != helmrelease.StatusFailed {
			continue
		}
		if target == nil || rel.Version > target.Version {
			target = rel
		}
	}
	if target == nil {
		return nil, errNoRollbackTarget
	}
	return target, nil
}

func isStalePendingRelease(rel *helmrelease.Release, now time.Time) (bool, time.Duration) {
	if rel == nil || rel.Info == nil || !rel.Info.Status.IsPending() {
		return false, 0
	}
	if rel.Info.LastDeployed.IsZero() {
		return false, 0
	}
	age := now.Sub(rel.Info.LastDeployed.Time)
	return age >= StalePendingReleaseAge, age
}

func (r *Reconciler) helmInstall(cfg *action.Configuration, ch *helmchart.Chart, ns string, vals map[string]interface{}) error {
	install := action.NewInstall(cfg)
	install.ReleaseName = releaseName
	install.Namespace = ns
	install.CreateNamespace = true
	install.Wait = false

	rel, err := install.Run(ch, vals)
	if err != nil {
		return fmt.Errorf("helm install: %w", err)
	}
	r.log.Info("helm install succeeded",
		zap.String("release", rel.Name),
		zap.Int("version", rel.Version),
	)
	return nil
}

func (r *Reconciler) helmUpgrade(cfg *action.Configuration, ch *helmchart.Chart, vals map[string]interface{}) error {
	upgrade := action.NewUpgrade(cfg)
	upgrade.Wait = false
	upgrade.MaxHistory = 10

	rel, err := upgrade.Run(releaseName, ch, vals)
	if err != nil {
		return fmt.Errorf("helm upgrade: %w", err)
	}
	r.log.Info("helm upgrade succeeded",
		zap.String("release", rel.Name),
		zap.Int("version", rel.Version),
	)
	return nil
}

func (r *Reconciler) removeKernelHotfixDrift(ctx context.Context, ns string) error {
	var st appsv1.StatefulSet
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kernel"}, &st); err != nil {
		return client.IgnoreNotFound(err)
	}

	if st.Labels[helm.ManagedByLabel] != helm.ManagedByValue {
		return nil
	}

	changed := false
	for i := range st.Spec.Template.Spec.Containers {
		container := &st.Spec.Template.Spec.Containers[i]
		if container.Name != "kernel" {
			continue
		}
		if containsKernelHotfix(container.Command) || containsKernelHotfix(container.Args) {
			container.Command = nil
			container.Args = nil
			changed = true
		}
		mounts, removed := filterHotfixVolumeMounts(container.VolumeMounts)
		if removed {
			container.VolumeMounts = mounts
			changed = true
		}
	}

	volumes, removed := filterHotfixVolumes(st.Spec.Template.Spec.Volumes)
	if removed {
		st.Spec.Template.Spec.Volumes = volumes
		changed = true
	}

	if !changed {
		return nil
	}

	if r.log != nil {
		r.log.Warn("removing kernel hotfix drift from StatefulSet before helm upgrade",
			zap.String("namespace", ns),
		)
	}
	return r.client.Update(ctx, &st)
}

// reconcileImmutableStatefulSetFields brings live StatefulSets into a shape the
// coming Helm upgrade can patch.
//
// Kubernetes forbids patching everything in a StatefulSet's spec except
// replicas, ordinals, template, updateStrategy, persistentVolumeClaimRetentionPolicy
// and minReadySeconds. A chart change to any other field — most commonly a
// volumeClaimTemplates storage bump — makes the whole `helm upgrade` fail at that
// resource, which leaves every service after it in the single combined release
// un-rolled. So the forbidden change is serviced here, before Helm sees it:
//
//   - A grown volumeClaimTemplates storage request and nothing else: the live
//     PVCs are expanded in place first (when their StorageClass allows it), then
//     the StatefulSet is deleted with an orphan propagation policy like any
//     other forbidden-field case below — its own template still names the old
//     size, so left alone the very next Helm upgrade hits the identical
//     rejection Kubernetes gave this one. Expanding first means the PVCs the
//     recreated StatefulSet re-adopts are already the right size.
//   - Anything else: the StatefulSet is deleted with an orphan propagation
//     policy so its pods keep running, and the Helm upgrade recreates it and
//     re-adopts them.
func (r *Reconciler) reconcileImmutableStatefulSetFields(ctx context.Context, ns string, ch *helmchart.Chart, vals map[string]interface{}) error {
	desired, err := renderStatefulSets(ch, ns, vals)
	if err != nil {
		return fmt.Errorf("rendering chart statefulsets: %w", err)
	}
	if len(desired) == 0 {
		return nil
	}

	var liveList appsv1.StatefulSetList
	if err := r.client.List(ctx, &liveList, &client.ListOptions{
		Namespace:     ns,
		LabelSelector: labels.SelectorFromSet(labels.Set{helm.ManagedByLabel: helm.ManagedByValue}),
	}); err != nil {
		return fmt.Errorf("listing statefulsets: %w", err)
	}

	for i := range liveList.Items {
		live := &liveList.Items[i]
		want := desired[live.Name]
		if want == nil {
			continue
		}

		forbidden, grown := immutableStatefulSetDiff(live, want)
		if len(forbidden) == 0 && len(grown) == 0 {
			continue
		}

		if len(grown) > 0 {
			r.expandStatefulSetPVCs(ctx, live, grown)
		}

		fields := forbidden
		if len(grown) > 0 {
			for name := range grown {
				fields = append(fields, "volumeClaimTemplates["+name+"].storage (grown)")
			}
		}
		r.log.Warn("recreating StatefulSet: the chart changes fields Kubernetes forbids patching; deleting with orphaned pods so the helm upgrade recreates it",
			zap.String("namespace", ns),
			zap.String("name", live.Name),
			zap.Strings("fields", fields),
		)
		if err := r.client.Delete(ctx, live, client.PropagationPolicy(metav1.DeletePropagationOrphan)); err != nil {
			return fmt.Errorf("orphan-deleting statefulset %s: %w", live.Name, err)
		}
	}
	return nil
}

// renderStatefulSets renders the chart client-side, exactly as the coming
// upgrade will render it, and returns the StatefulSets it declares by name.
func renderStatefulSets(ch *helmchart.Chart, ns string, vals map[string]interface{}) (map[string]*appsv1.StatefulSet, error) {
	install := action.NewInstall(new(action.Configuration))
	install.ReleaseName = releaseName
	install.Namespace = ns
	install.DryRun = true
	install.ClientOnly = true

	rel, err := install.Run(ch, vals)
	if err != nil {
		return nil, fmt.Errorf("helm dry-run render: %w", err)
	}

	out := map[string]*appsv1.StatefulSet{}
	for _, doc := range releaseutil.SplitManifests(rel.Manifest) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := k8syaml.Unmarshal([]byte(doc), &head); err != nil || head.Kind != "StatefulSet" {
			continue
		}
		var st appsv1.StatefulSet
		if err := k8syaml.Unmarshal([]byte(doc), &st); err != nil {
			return nil, fmt.Errorf("decoding rendered statefulset: %w", err)
		}
		out[st.Name] = &st
	}
	return out, nil
}

// immutableStatefulSetDiff compares the StatefulSet spec fields Kubernetes
// refuses to patch on a live object. It returns the ones that genuinely differ,
// and separately, keyed by volumeClaimTemplate name, the storage requests that
// only grew — the one forbidden change that can be serviced by expanding the
// live PVCs instead of recreating the StatefulSet.
//
// Only fields the chart actually sets are compared. A live StatefulSet carries
// server-side defaults (volumeMode, storageClassName, the whole status subtree)
// that the rendered manifest omits, and comparing those would report drift on
// every reconcile and orphan-delete the fleet in a loop.
func immutableStatefulSetDiff(live, desired *appsv1.StatefulSet) ([]string, map[string]resource.Quantity) {
	var forbidden []string
	grown := map[string]resource.Quantity{}

	if desired.Spec.ServiceName != "" && desired.Spec.ServiceName != live.Spec.ServiceName {
		forbidden = append(forbidden, "spec.serviceName")
	}
	if desired.Spec.PodManagementPolicy != "" && desired.Spec.PodManagementPolicy != live.Spec.PodManagementPolicy {
		forbidden = append(forbidden, "spec.podManagementPolicy")
	}
	if desired.Spec.Selector != nil && !apiequality.Semantic.DeepEqual(desired.Spec.Selector, live.Spec.Selector) {
		forbidden = append(forbidden, "spec.selector")
	}

	if len(desired.Spec.VolumeClaimTemplates) != len(live.Spec.VolumeClaimTemplates) {
		return append(forbidden, "spec.volumeClaimTemplates"), nil
	}

	liveTemplates := make(map[string]*corev1.PersistentVolumeClaim, len(live.Spec.VolumeClaimTemplates))
	for i := range live.Spec.VolumeClaimTemplates {
		liveTemplates[live.Spec.VolumeClaimTemplates[i].Name] = &live.Spec.VolumeClaimTemplates[i]
	}

	for i := range desired.Spec.VolumeClaimTemplates {
		want := &desired.Spec.VolumeClaimTemplates[i]
		have, ok := liveTemplates[want.Name]
		if !ok {
			forbidden = append(forbidden, "spec.volumeClaimTemplates["+want.Name+"]")
			continue
		}
		if !apiequality.Semantic.DeepEqual(want.Spec.AccessModes, have.Spec.AccessModes) ||
			(want.Spec.StorageClassName != nil && !apiequality.Semantic.DeepEqual(want.Spec.StorageClassName, have.Spec.StorageClassName)) ||
			(want.Spec.VolumeMode != nil && !apiequality.Semantic.DeepEqual(want.Spec.VolumeMode, have.Spec.VolumeMode)) ||
			(want.Spec.Selector != nil && !apiequality.Semantic.DeepEqual(want.Spec.Selector, have.Spec.Selector)) {
			forbidden = append(forbidden, "spec.volumeClaimTemplates["+want.Name+"].spec")
			continue
		}

		wantSize := want.Spec.Resources.Requests[corev1.ResourceStorage]
		haveSize := have.Spec.Resources.Requests[corev1.ResourceStorage]
		switch wantSize.Cmp(haveSize) {
		case 1:
			grown[want.Name] = wantSize
		case -1:
			// A PVC cannot shrink, so there is nothing to patch: recreate the
			// StatefulSet so at least its template matches the chart again.
			forbidden = append(forbidden, "spec.volumeClaimTemplates["+want.Name+"].spec.resources.requests.storage")
		}
	}

	if len(forbidden) > 0 {
		return forbidden, nil
	}
	return nil, grown
}

// expandStatefulSetPVCs grows the live PVCs of a StatefulSet whose chart
// volumeClaimTemplates asked for more storage. A StatefulSet's PVCs carry the
// labels of its selector and are named <template>-<statefulset>-<ordinal>.
//
// Nothing here fails the reconcile: a PVC that cannot be grown is a problem for
// that one volume, not a reason to leave the rest of the fleet un-rolled.
func (r *Reconciler) expandStatefulSetPVCs(ctx context.Context, st *appsv1.StatefulSet, grown map[string]resource.Quantity) {
	selector, err := metav1.LabelSelectorAsSelector(st.Spec.Selector)
	if err != nil {
		r.log.Warn("cannot expand StatefulSet PVCs: unusable selector",
			zap.String("name", st.Name),
			zap.Error(err),
		)
		return
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := r.client.List(ctx, &pvcs, &client.ListOptions{
		Namespace:     st.Namespace,
		LabelSelector: selector,
	}); err != nil {
		r.log.Warn("cannot expand StatefulSet PVCs: listing them failed",
			zap.String("name", st.Name),
			zap.Error(err),
		)
		return
	}

	for template, size := range grown {
		prefix := template + "-" + st.Name + "-"
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			if !strings.HasPrefix(pvc.Name, prefix) {
				continue
			}
			r.expandPVC(ctx, pvc, st.Name, size)
		}
	}
}

func (r *Reconciler) expandPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, stName string, size resource.Quantity) {
	current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.Cmp(current) <= 0 {
		return
	}
	if !r.storageClassAllowsExpansion(ctx, pvc) {
		return
	}

	patch := client.MergeFrom(pvc.DeepCopy())
	if pvc.Spec.Resources.Requests == nil {
		pvc.Spec.Resources.Requests = corev1.ResourceList{}
	}
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = size
	if err := r.client.Patch(ctx, pvc, patch); err != nil {
		r.log.Warn("expanding PVC failed; StatefulSet left as is",
			zap.String("statefulset", stName),
			zap.String("pvc", pvc.Name),
			zap.Error(err),
		)
		return
	}

	r.log.Warn("expanded StatefulSet PVC to the size the chart now asks for",
		zap.String("statefulset", stName),
		zap.String("pvc", pvc.Name),
		zap.String("from", current.String()),
		zap.String("to", size.String()),
	)
}

// storageClassAllowsExpansion reports whether the PVC's StorageClass permits
// growing a bound volume. An unknown or non-expandable StorageClass is a loud
// warning and a skip, never an error: the operator must not fail the whole
// reconcile over one volume it cannot grow.
func (r *Reconciler) storageClassAllowsExpansion(ctx context.Context, pvc *corev1.PersistentVolumeClaim) bool {
	name := ""
	if pvc.Spec.StorageClassName != nil {
		name = *pvc.Spec.StorageClassName
	}
	if name == "" {
		r.log.Warn("not expanding PVC: it names no StorageClass, so volume expansion cannot be confirmed safe",
			zap.String("pvc", pvc.Name),
		)
		return false
	}

	var sc storagev1.StorageClass
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, &sc); err != nil {
		r.log.Warn("not expanding PVC: its StorageClass could not be read",
			zap.String("pvc", pvc.Name),
			zap.String("storage_class", name),
			zap.Error(err),
		)
		return false
	}

	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		r.log.Warn("not expanding PVC: its StorageClass does not allow volume expansion; grow the volume by hand or recreate the StatefulSet",
			zap.String("pvc", pvc.Name),
			zap.String("storage_class", name),
		)
		return false
	}
	return true
}

func containsKernelHotfix(values []string) bool {
	for _, value := range values {
		if strings.Contains(value, "/hotfix") ||
			strings.Contains(value, "kernel-261.jar") ||
			strings.Contains(value, "/opt/schematic-hotfix") {
			return true
		}
	}
	return false
}

func filterHotfixVolumeMounts(mounts []corev1.VolumeMount) ([]corev1.VolumeMount, bool) {
	filtered := mounts[:0]
	removed := false
	for _, mount := range mounts {
		if strings.EqualFold(mount.Name, "kernel-hotfix") ||
			strings.Contains(strings.ToLower(mount.Name), "hotfix") ||
			mount.MountPath == "/hotfix" {
			removed = true
			continue
		}
		filtered = append(filtered, mount)
	}
	return filtered, removed
}

func filterHotfixVolumes(volumes []corev1.Volume) ([]corev1.Volume, bool) {
	filtered := volumes[:0]
	removed := false
	for _, volume := range volumes {
		if strings.EqualFold(volume.Name, "kernel-hotfix") ||
			strings.Contains(strings.ToLower(volume.Name), "hotfix") ||
			(volume.HostPath != nil && volume.HostPath.Path == "/opt/schematic-hotfix") {
			removed = true
			continue
		}
		filtered = append(filtered, volume)
	}
	return filtered, removed
}

// pruneStaleDeployments deletes Deployments in the schematic namespace that are
// managed by the chart but no longer expected (e.g. after topology scale-down).
//
// Ownership is established solely by the chart's managed-by label. There used to
// be a second gate on app.kubernetes.io/instance == releaseName, but the chart
// never applies that label, so it dropped every candidate the selector found.
func (r *Reconciler) pruneStaleDeployments(ctx context.Context, nodeCount int) error {
	expected := r.generator.ExpectedDeploymentNames(r.cfg, nodeCount)

	ns := r.cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}

	var deployList appsv1.DeploymentList
	if err := r.client.List(ctx, &deployList, &client.ListOptions{
		Namespace:     ns,
		LabelSelector: labels.SelectorFromSet(labels.Set{helm.ManagedByLabel: helm.ManagedByValue}),
	}); err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}

	for i := range deployList.Items {
		dep := &deployList.Items[i]
		name := dep.Name
		if expected[name] {
			continue
		}
		r.log.Info("pruning stale deployment", zap.String("name", name))
		if err := r.client.Delete(ctx, dep); err != nil {
			r.log.Warn("failed to delete stale deployment", zap.String("name", name), zap.Error(err))
		}
		// Also clean up the associated Service and ConfigMap.
		svc := &corev1.Service{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, svc); err == nil {
			_ = r.client.Delete(ctx, svc)
		}
		cm := &corev1.ConfigMap{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: name + "-config", Namespace: ns}, cm); err == nil {
			_ = r.client.Delete(ctx, cm)
		}
		// Services whose config holds private key material (gateway, kmaker)
		// package it as a Secret under the same name.
		sec := &corev1.Secret{}
		if err := r.client.Get(ctx, types.NamespacedName{Name: name + "-config", Namespace: ns}, sec); err == nil {
			_ = r.client.Delete(ctx, sec)
		}
	}
	return nil
}

// inClusterGetter implements action.RESTClientGetter using the in-cluster
// rest.Config supplied by controller-runtime.
type inClusterGetter struct {
	restCfg   *rest.Config
	mapper    apimeta.RESTMapper
	namespace string
}

func (h *inClusterGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	cfg := clientcmdapi.NewConfig()

	cluster := clientcmdapi.NewCluster()
	cluster.Server = h.restCfg.Host
	cluster.InsecureSkipTLSVerify = h.restCfg.TLSClientConfig.Insecure
	cluster.CertificateAuthorityData = h.restCfg.TLSClientConfig.CAData
	cfg.Clusters["in-cluster"] = cluster

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.Token = h.restCfg.BearerToken
	cfg.AuthInfos["in-cluster"] = authInfo

	ctx := clientcmdapi.NewContext()
	ctx.Cluster = "in-cluster"
	ctx.AuthInfo = "in-cluster"
	ctx.Namespace = h.namespace
	cfg.Contexts["in-cluster"] = ctx
	cfg.CurrentContext = "in-cluster"

	return clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{})
}

func (h *inClusterGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(rest.CopyConfig(h.restCfg))
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (h *inClusterGetter) ToRESTMapper() (apimeta.RESTMapper, error) {
	return h.mapper, nil
}

func (h *inClusterGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(h.restCfg), nil
}
