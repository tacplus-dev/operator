// Package upgrade implements automatic version detection for cavalier service images.
//
// Version tag format: v{MAJOR}.{MINOR}.{PROTO_PATCH}-{BRANCH}.{LOCAL_PATCH}
// e.g. "v1.2.12-main.3"
//
// PROTO_PATCH: the patch level of the proto repo (cavalier/cavalier) at build time.
// BRANCH: the git branch of the service repo at build time (e.g. "main", "v1-2").
// LOCAL_PATCH: auto-incremented per-service build counter at the given proto patch.
//
// The Checker polls the registry for each known service and caches the latest
// LOCAL_PATCH. It signals the notify channel whenever any cached value changes.
//
// Pinning: if localPatch > 0 in the version spec, no polling is performed.
package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	registryHost   = "registry.gitlab.com"
	registryBase   = "secondstarcavalier/schematic/platform/services"
	operatorBase   = "secondstarcavalier/schematic/platform/cavalier"
	gitlabTokenURL = "https://gitlab.com/jwt/auth"

	PollInterval    = 60 * time.Second
	DevPollInterval = 10 * time.Second
)

// ServiceRef identifies a service image in the registry.
type ServiceRef struct {
	// Proj is the GitLab registry project name (under services/).
	// e.g. "kernel", "anthropic-proxy".
	Proj string
	// Svc is the image name within the project.
	// e.g. "kernel", "anthropic".
	Svc string
}

// Checker polls the container registry for new local patches per service.
// It resolves the latest LOCAL_PATCH for each registered service at a given
// (PROTO_PATCH, BRANCH) and signals via Notify() when anything changes.
type Checker struct {
	versionLock  string // "MAJOR.MINOR", e.g. "1.2"
	series       string // "MAJOR-MINOR", e.g. "1-2"
	protoPatch   int    // pinned PROTO_PATCH; 0 = any (use highest found)
	branch       string // e.g. "main"
	localPatch   int    // pinned LOCAL_PATCH; 0 = auto (use latest)
	fallbackTag  string // operator's own version tag, used when resolution fails
	services     []ServiceRef
	k8sClient    client.Client
	operatorNS   string
	log          *zap.Logger
	pollInterval time.Duration

	mu     sync.RWMutex
	cache  map[string]int32 // "proj/svc" → latest LOCAL_PATCH
	notify chan struct{}
}

// New creates a Checker.
//
//   - versionLock: "MAJOR.MINOR" (e.g. "1.2")
//   - series: "MAJOR-MINOR" (e.g. "1-2")
//   - protoPatch: PROTO_PATCH from the operator binary version (0 = use any)
//   - branch: BRANCH from the operator binary version (e.g. "main")
//   - localPatch: if > 0, all services are pinned to this LOCAL_PATCH and no polling runs
//   - fallbackTag: operator's own version tag (e.g. "v1.2.27-main.1"), used as fallback when resolution fails
//   - services: list of service images to monitor
//   - interval: how often to poll the registry (use DevPollInterval in dev mode)
func New(versionLock, series string, protoPatch int, branch string, localPatch int, fallbackTag string, services []ServiceRef, k8sClient client.Client, operatorNS string, log *zap.Logger, interval time.Duration) *Checker {
	return &Checker{
		versionLock:  versionLock,
		series:       series,
		protoPatch:   protoPatch,
		branch:       branch,
		localPatch:   localPatch,
		fallbackTag:  fallbackTag,
		services:     services,
		k8sClient:    k8sClient,
		operatorNS:   operatorNS,
		log:          log,
		pollInterval: interval,
		cache:        make(map[string]int32),
		notify:       make(chan struct{}, 1),
	}
}

// Notify returns a channel that receives a signal whenever any service has a
// newer LOCAL_PATCH available. The channel is buffered (size 1); missed signals
// are coalesced.
func (c *Checker) Notify() <-chan struct{} {
	return c.notify
}

// Resolve does one synchronous resolution of all services and caches the results.
// Returns the resolved local-patch map. Safe to call before Run.
func (c *Checker) Resolve(ctx context.Context) (map[string]int32, error) {
	creds, err := c.registryCreds(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting registry creds: %w", err)
	}
	resolved, err := c.resolveAll(ctx, creds)
	if err != nil {
		return nil, err
	}
	c.updateCache(resolved)
	return resolved, nil
}

// LatestLocalPatch returns the cached latest LOCAL_PATCH for a service.
// Returns the pinned localPatch if > 0. Returns 0 if not yet resolved.
func (c *Checker) LatestLocalPatch(proj, svc string) int32 {
	if c.localPatch > 0 {
		return int32(c.localPatch)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache[cacheKey(proj, svc)]
}

// LatestLocalPatchForOperator returns the latest LOCAL_PATCH for the operator image itself.
func (c *Checker) LatestLocalPatchForOperator() int32 {
	if c.localPatch > 0 {
		return int32(c.localPatch)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cache["operator"]
}

// Run polls the registry on pollInterval until ctx is cancelled.
// Call Resolve first to populate the initial cache.
func (c *Checker) Run(ctx context.Context) {
	if c.localPatch > 0 {
		c.log.Info("version pinned, auto-upgrade disabled",
			zap.String("branch", c.branch),
			zap.Int("proto_patch", c.protoPatch),
			zap.Int("local_patch", c.localPatch),
		)
		return
	}

	c.log.Info("starting auto-upgrade checker",
		zap.String("branch", c.branch),
		zap.Int("proto_patch", c.protoPatch),
		zap.Duration("interval", c.pollInterval),
	)

	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			creds, err := c.registryCreds(ctx)
			if err != nil {
				c.log.Warn("auto-upgrade: failed to get registry creds", zap.Error(err))
				continue
			}
			resolved, err := c.resolveAll(ctx, creds)
			if err != nil {
				c.log.Warn("auto-upgrade: resolution failed", zap.Error(err))
				continue
			}
			if c.updateCache(resolved) {
				c.log.Info("auto-upgrade: new versions available")
				select {
				case c.notify <- struct{}{}:
				default:
				}
			}
		}
	}
}

// imageTag builds a full image tag from the version components.
// e.g. (12, "main", 3) → "v1.2.12-main.3"
func (c *Checker) imageTag(protoPatch int, branch string, localPatch int32) string {
	return fmt.Sprintf("v%s.%d-%s.%d", c.versionLock, protoPatch, branch, localPatch)
}

// ServiceTag returns the full image tag for a service using cached resolution.
// Falls back to fallbackTag (the operator's own version) if not yet resolved.
func (c *Checker) ServiceTag(proj, svc string) string {
	lp := c.LatestLocalPatch(proj, svc)
	if lp == 0 {
		return c.fallbackTag
	}
	return c.imageTag(c.protoPatch, c.branch, lp)
}

// OperatorTag returns the full image tag for the operator itself using cached resolution.
// Falls back to fallbackTag (the operator's own version) if not yet resolved.
func (c *Checker) OperatorTag() string {
	lp := c.LatestLocalPatchForOperator()
	if lp == 0 {
		return c.fallbackTag
	}
	return c.imageTag(c.protoPatch, c.branch, lp)
}

// resolveAll queries the registry for all registered services plus the operator.
func (c *Checker) resolveAll(ctx context.Context, creds registryCreds) (map[string]int32, error) {
	result := make(map[string]int32)

	// Resolve each service.
	for _, svc := range c.services {
		repo := fmt.Sprintf("%s/%s/%s-%s", registryBase, svc.Proj, svc.Svc, c.series)
		lp, err := c.resolveRepo(ctx, repo, creds)
		if err != nil {
			c.log.Warn("auto-upgrade: failed to resolve service",
				zap.String("proj", svc.Proj),
				zap.String("svc", svc.Svc),
				zap.Error(err),
			)
			continue
		}
		if lp > 0 {
			result[cacheKey(svc.Proj, svc.Svc)] = lp
		}
	}

	// Resolve the operator itself.
	opRepo := fmt.Sprintf("%s/cavalier-operator", operatorBase)
	opLP, err := c.resolveRepo(ctx, opRepo, creds)
	if err != nil {
		c.log.Warn("auto-upgrade: failed to resolve operator image", zap.Error(err))
	} else if opLP > 0 {
		result["operator"] = opLP
	}

	return result, nil
}

// resolveRepo queries a registry repo and returns the highest LOCAL_PATCH
// matching (PROTO_PATCH, BRANCH). Returns 0 if no matching tags found.
func (c *Checker) resolveRepo(ctx context.Context, repo string, creds registryCreds) (int32, error) {
	token, err := c.registryToken(ctx, repo, creds)
	if err != nil {
		return 0, err
	}
	tags, err := c.listTags(ctx, repo, token)
	if err != nil {
		return 0, err
	}
	return selectLatest(tags, c.versionLock, c.protoPatch, c.branch), nil
}

// updateCache merges resolved into the cache. Returns true if any value changed.
func (c *Checker) updateCache(resolved map[string]int32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	for k, v := range resolved {
		if c.cache[k] != v {
			c.cache[k] = v
			changed = true
		}
	}
	return changed
}

func cacheKey(proj, svc string) string {
	return proj + "/" + svc
}

type registryCreds struct{ username, password string }

func (c *Checker) registryCreds(ctx context.Context) (registryCreds, error) {
	var secret corev1.Secret
	if err := c.k8sClient.Get(ctx, types.NamespacedName{
		Namespace: c.operatorNS,
		Name:      "gitlab-registry",
	}, &secret); err != nil {
		return registryCreds{}, fmt.Errorf("getting gitlab-registry secret: %w", err)
	}
	raw, ok := secret.Data[".dockerconfigjson"]
	if !ok {
		return registryCreds{}, fmt.Errorf("gitlab-registry secret missing .dockerconfigjson")
	}
	var dc struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &dc); err != nil {
		return registryCreds{}, fmt.Errorf("parsing docker config: %w", err)
	}
	entry, ok := dc.Auths[registryHost]
	if !ok {
		return registryCreds{}, fmt.Errorf("no credentials for %s", registryHost)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return registryCreds{}, fmt.Errorf("decoding auth: %w", err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return registryCreds{}, fmt.Errorf("malformed auth")
	}
	return registryCreds{username: parts[0], password: parts[1]}, nil
}

func (c *Checker) registryToken(ctx context.Context, repo string, creds registryCreds) (string, error) {
	url := fmt.Sprintf("%s?service=container_registry&scope=repository:%s:pull", gitlabTokenURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(creds.username, creds.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request returned %s", resp.Status)
	}
	var tr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.Token == "" {
		return "", fmt.Errorf("invalid token response")
	}
	return tr.Token, nil
}

func (c *Checker) listTags(ctx context.Context, repo, token string) ([]string, error) {
	var allTags []string
	url := fmt.Sprintf("https://%s/v2/%s/tags/list?n=10000", registryHost, repo)

	for url != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tags/list returned %s", resp.Status)
		}
		var tr struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &tr); err != nil {
			return nil, fmt.Errorf("parsing tags: %w", err)
		}
		allTags = append(allTags, tr.Tags...)

		// Follow pagination Link header if present.
		url = ""
		if link := resp.Header.Get("Link"); link != "" {
			if start := strings.Index(link, "<"); start != -1 {
				if end := strings.Index(link[start:], ">"); end != -1 {
					url = link[start+1 : start+end]
				}
			}
		}
	}
	return allTags, nil
}

// selectLatest returns the highest LOCAL_PATCH from tags matching
// v{versionLock}.{protoPatch}-{branch}.{N}.
// Returns 0 if no matching tags are found.
func selectLatest(tags []string, versionLock string, protoPatch int, branch string) int32 {
	escaped := regexp.QuoteMeta(fmt.Sprintf("v%s.%d-%s.", versionLock, protoPatch, branch))
	re := regexp.MustCompile(`^` + escaped + `(\d+)$`)

	var best int32
	for _, t := range tags {
		m := re.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		p, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if int32(p) > best {
			best = int32(p)
		}
	}
	return best
}
