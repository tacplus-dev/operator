// Package reconciler implements the in-cluster operator reconciliation loop.
//
// When mode = IN_CLUSTER, the reconciler:
//  1. Watches the Kubernetes node count (schedulable worker nodes).
//  2. On node count change or periodic tick, regenerates the cavalier Helm chart.
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
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"helm.sh/helm/v3/pkg/action"
	helmchart "helm.sh/helm/v3/pkg/chart"
	helmloader "helm.sh/helm/v3/pkg/chart/loader"
	helmdriver "helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/helm"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/upgrade"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

// ReconcileInterval is how often the reconciler re-checks cluster state
// even when no watch events fire.
const ReconcileInterval = 30 * time.Second

const releaseName = "cavalier"

// ServiceConfigProvider returns per-service Helm values (map of service name →
// {"config": yaml-string}) on each reconcile. It is called once per reconcile
// loop iteration. Returning nil, nil means "no overrides — use chart defaults".
type ServiceConfigProvider func() (map[string]interface{}, error)

// Reconciler manages the cavalier Helm release inside the cluster.
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
}

// New creates a Reconciler. Call Run to start the reconciliation loop.
// configProvider is called on each reconcile to obtain per-service Helm values;
// pass nil if service configs are not available (charts will deploy with empty configs).
// operatorNS is the namespace where the operator pod runs (used for reading
// the gitlab-registry secret when auto_upgrade is enabled).
// checker resolves per-service image tags; the generator uses it as a TagResolver.
func New(cfg *protoOperator.Config, configProvider ServiceConfigProvider, checker *upgrade.Checker, mgr manager.Manager, log *zap.Logger, operatorNS, series, versionLock string) *Reconciler {
	return &Reconciler{
		cfg:            cfg,
		configProvider: configProvider,
		checker:        checker,
		generator:      helm.New(series, versionLock, checker),
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
	return "cavalier"
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
		Name:      "cavalier-operator",
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
		`{"spec":{"template":{"spec":{"containers":[{"name":"cavalier-operator","image":%q}]}}}}`,
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
	if r.configProvider != nil {
		vals, err := r.configProvider()
		if err != nil {
			return fmt.Errorf("building service configs: %w", err)
		}
		svcValues = vals
		r.log.Info("built service configs", zap.Int("services", len(svcValues)))
	}

	if err := r.apply(ctx, charts, svcValues); err != nil {
		return fmt.Errorf("applying charts: %w", err)
	}

	// Prune deployments that are no longer in the chart (e.g. after topology scale-down).
	if err := r.pruneStaleDeployments(ctx, nodeCount); err != nil {
		r.log.Warn("failed to prune stale deployments", zap.Error(err))
	}

	r.log.Info("reconcile complete", zap.Int("files", len(charts.Files)))
	return nil
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

// apply installs or upgrades the "cavalier" Helm release using the in-memory
// chart files produced by the generator. svcValues overrides values.yaml defaults
// (typically per-service config YAML strings); nil is treated as an empty map.
func (r *Reconciler) apply(_ context.Context, charts *helm.Charts, svcValues map[string]interface{}) error {
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

	histAction := action.NewHistory(actionCfg)
	histAction.Max = 1
	_, histErr := histAction.Run(releaseName)

	if svcValues == nil {
		svcValues = map[string]interface{}{}
	}

	switch {
	case histErr == helmdriver.ErrReleaseNotFound:
		return r.helmInstall(actionCfg, ch, ns, svcValues)
	case histErr != nil:
		return fmt.Errorf("querying helm release history: %w", histErr)
	default:
		return r.helmUpgrade(actionCfg, ch, svcValues)
	}
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
	// Reconciliation must make workloads match the generated chart, including
	// removing any out-of-band pod command/volume mutations.
	upgrade.Force = true
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

// pruneStaleDeployments deletes Deployments in the cavalier namespace that are
// managed by the chart but no longer expected (e.g. after topology scale-down).
func (r *Reconciler) pruneStaleDeployments(ctx context.Context, nodeCount int) error {
	expected := r.generator.ExpectedDeploymentNames(r.cfg, nodeCount)

	ns := r.cfg.Namespace
	if ns == "" {
		ns = "cavalier"
	}

	var deployList appsv1.DeploymentList
	if err := r.client.List(ctx, &deployList, &client.ListOptions{
		Namespace:     ns,
		LabelSelector: labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": "Helm"}),
	}); err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}

	for i := range deployList.Items {
		dep := &deployList.Items[i]
		name := dep.Name
		if expected[name] {
			continue
		}
		// Only delete deployments that belong to the cavalier helm release.
		if dep.Labels["app.kubernetes.io/instance"] != releaseName {
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
