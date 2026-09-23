package bootstrap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// serviceDeps holds the parsed dependency graph from the tacplus registry.
// Each key is a service name (matching deployed service names like "graphdb",
// "argus", etc.), and the value is the sorted set of service names that
// service must be authorized to call.
type serviceDeps struct {
	deps map[string][]string
}

// registryNode mirrors the subset of tacplus PackageNode fields we need.
// Using a lightweight struct instead of the full proto to avoid pulling in
// the entire proto dependency chain for JSON unmarshaling.
type registryNode struct {
	Name               string              `json:"name"`
	FullPath           string              `json:"full_path"`
	Children           []*registryNode     `json:"children"`
	Boundaries         []registryBoundary  `json:"boundaries"`
	Services           []registryService   `json:"services"`
	ClientDependencies []registryClientDep `json:"client_dependencies"`
	EventPublishes     []registryEventPub  `json:"event_publishes"`
	EventWatches       []registryEventPub  `json:"event_watches"`
}

type registryBoundary struct {
	Labels []string `json:"label"`
}

type registryService struct {
	Name       string `json:"name"`
	Package    string `json:"package"`
	GrpcSvc    string `json:"grpc_service"`
	HostLabel  string `json:"host_label"`
	EventLabel string `json:"event_label"`
	// TenantOwned mirrors ServiceInfo.tenant_owned, which tacplus emits from
	// the service-level (spec.service).tenant_owned annotation. True means the
	// operator runs one instance of this service per tenant. Absent (the proto3
	// default, and what every entry in today's registry carries) means shared.
	TenantOwned bool `json:"tenant_owned"`
	// PinnedTenant mirrors ServiceInfo.pinned_tenant, which tacplus emits from
	// the service-level (spec.service).pinned_tenant annotation. Non-empty
	// means this tenant-owned service runs as exactly one instance, in that
	// tenant, instead of one per configured tenant. Empty (the proto3 default,
	// and what every entry in today's registry carries) means per-tenant.
	PinnedTenant string `json:"pinned_tenant"`
}

type registryClientDep struct {
	SourcePackage string `json:"source_package"`
	SurfaceKind   string `json:"surface_kind"`
	TargetPackage string `json:"target_package"`
	TargetService string `json:"target_service"`
	Wildcard      bool   `json:"wildcard"`
}

type registryEventPub struct {
	BrokerLabel string `json:"broker_label"`
}

// loadServiceDeps parses the embedded tacplus registry and derives per-service
// authorized_services from the declared dependency graph:
//   - boundary labels → which services host this service's state/config
//   - s2s client_dependencies → resolved through vservice host_label to physical services
//   - event_publishes/event_watches → broker_label identifies the event broker
//   - always includes self + kernel
//
// Only returns deps for services in the known set (kernelAuthenticatedServiceNames).
// Never emits SERVICE_NAME_ANY.
func loadServiceDeps() (*serviceDeps, error) {
	var root registryNode
	if err := json.Unmarshal(registryJSON, &root); err != nil {
		return nil, fmt.Errorf("parsing registry.json: %w", err)
	}

	// Phase 1: Build vservice grpc_service → physical service mapping from
	// host_label and event_label on vservice ServiceInfo entries.
	//   e.g. "agentsensor.StratumQuery" → "stratum"
	//        "agentsensor.AgentEventBusStateService" → "eventbus"
	grpcToPhysical := make(map[string]string)
	walkNodes(&root, func(node *registryNode) {
		if !strings.HasPrefix(node.FullPath, "vservices/") {
			return
		}
		for _, svc := range node.Services {
			if svc.HostLabel != "" {
				grpcToPhysical[svc.GrpcSvc] = svc.HostLabel
			}
			if svc.EventLabel != "" {
				grpcToPhysical[svc.GrpcSvc] = svc.EventLabel
			}
		}
	})

	// Phase 2: For each physical service node, collect dependencies.
	result := make(map[string]map[string]bool)
	walkNodes(&root, func(node *registryNode) {
		if !strings.HasPrefix(node.FullPath, "services/") {
			return
		}
		svcName := node.Name
		if !isKernelAuthenticatedService(svcName) && svcName != kernelServiceName {
			return
		}

		deps := make(map[string]bool)
		// Always: self + kernel.
		deps[svcName] = true
		deps[kernelServiceName] = true

		// Boundary labels → hosting service dependencies.
		for _, b := range node.Boundaries {
			for _, label := range b.Labels {
				if label != "" && isKernelAuthenticatedService(label) {
					deps[label] = true
				}
			}
		}

		// S2S client dependencies → resolve targets to physical services.
		for _, cd := range node.ClientDependencies {
			if cd.SurfaceKind != "s2s" || cd.Wildcard {
				continue
			}
			target := resolveTarget(cd, grpcToPhysical)
			if target != "" && (isKernelAuthenticatedService(target) || target == kernelServiceName) {
				deps[target] = true
			}
		}

		// Event publishes → broker service.
		for _, ep := range node.EventPublishes {
			if ep.BrokerLabel != "" && isKernelAuthenticatedService(ep.BrokerLabel) {
				deps[ep.BrokerLabel] = true
			}
		}

		// Event watches → broker service.
		for _, ew := range node.EventWatches {
			if ew.BrokerLabel != "" && isKernelAuthenticatedService(ew.BrokerLabel) {
				deps[ew.BrokerLabel] = true
			}
		}

		result[svcName] = deps
	})

	// Convert to sorted slices.
	sd := &serviceDeps{deps: make(map[string][]string, len(result))}
	for svc, depSet := range result {
		list := make([]string, 0, len(depSet))
		for d := range depSet {
			list = append(list, d)
		}
		sort.Strings(list)
		sd.deps[svc] = list
	}
	return sd, nil
}

// resolveTarget maps a client dependency target to a physical service name.
func resolveTarget(cd registryClientDep, grpcToPhysical map[string]string) string {
	// First: check if the target is a vservice service with a host_label mapping.
	grpcKey := cd.TargetPackage + "." + cd.TargetService
	if physical, ok := grpcToPhysical[grpcKey]; ok {
		return physical
	}
	// Fallback: target_package is itself a physical service (e.g. "kernel").
	return cd.TargetPackage
}

// authorizedServicesFromRegistry returns the authorized service list for a
// service, derived from the registry dependency graph. Falls back to the
// minimum [self, kernel] if the service is not found in the registry.
func (sd *serviceDeps) authorizedServicesFor(svc string) []string {
	if deps, ok := sd.deps[svc]; ok {
		return deps
	}
	// Fallback for services not in the registry (e.g. not yet generated).
	return authorizedServicesFor(svc)
}

// tenantOwnedServices returns the set of physical service names that
// registry.json marks tenant_owned. This annotation is the ONLY source of
// truth for "is this service tenant-owned" — the operator carries no parallel
// allowlist or config flag that could disagree with it.
//
// The annotation is file-level in the proto ((spec.service) on the package's
// single service block), so tacplus stamps it onto every ServiceInfo the file
// produces; one entry carrying it marks the whole service node.
func tenantOwnedServices() (map[string]bool, error) {
	return tenantOwnedServicesFrom(registryJSON)
}

// TenantOwnedServices exposes the tenant_owned set to the helm generator and
// the reconciler, which cannot read the embedded registry themselves: this
// package imports both, so the dependency only runs one way.
func TenantOwnedServices() (map[string]bool, error) {
	return tenantOwnedServices()
}

func tenantOwnedServicesFrom(data []byte) (map[string]bool, error) {
	var root registryNode
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing registry.json: %w", err)
	}
	out := make(map[string]bool)
	walkNodes(&root, func(node *registryNode) {
		if !strings.HasPrefix(node.FullPath, "services/") {
			return
		}
		for _, svc := range node.Services {
			if svc.TenantOwned {
				out[node.Name] = true
				return
			}
		}
	})
	return out, nil
}

// pinnedTenantServices returns, for each tenant-owned service that registry.json
// pins, the single tenant it is pinned to. Like tenantOwnedServices this
// annotation is the ONLY source of truth: the operator carries no list of
// reserved tenants or singleton services that could disagree with the proto.
//
// A service absent from this map is the fleet's normal case — tenant-owned
// means one instance per configured tenant. A service present in it gets
// exactly one instance, in the named tenant, and reconciliation fails if that
// tenant is not configured.
func pinnedTenantServices() (map[string]string, error) {
	return pinnedTenantServicesFrom(registryJSON)
}

// PinnedTenantServices exposes the pinned set to the helm generator and the
// reconciler, for the same reason TenantOwnedServices does.
func PinnedTenantServices() (map[string]string, error) {
	return pinnedTenantServices()
}

func pinnedTenantServicesFrom(data []byte) (map[string]string, error) {
	var root registryNode
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing registry.json: %w", err)
	}
	out := make(map[string]string)
	walkNodes(&root, func(node *registryNode) {
		if !strings.HasPrefix(node.FullPath, "services/") {
			return
		}
		for _, svc := range node.Services {
			if svc.PinnedTenant != "" {
				out[node.Name] = svc.PinnedTenant
				return
			}
		}
	})
	return out, nil
}

func walkNodes(node *registryNode, fn func(*registryNode)) {
	fn(node)
	for _, child := range node.Children {
		walkNodes(child, fn)
	}
}
