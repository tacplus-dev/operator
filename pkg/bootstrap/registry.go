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
	Tag string `json:"tag"`
}

type registryService struct {
	Name       string `json:"name"`
	Package    string `json:"package"`
	GrpcSvc    string `json:"grpc_service"`
	HostTag    string `json:"host_tag"`
	EventTag   string `json:"event_tag"`
}

type registryClientDep struct {
	SourcePackage string `json:"source_package"`
	SurfaceKind   string `json:"surface_kind"`
	TargetPackage string `json:"target_package"`
	TargetService string `json:"target_service"`
	Wildcard      bool   `json:"wildcard"`
}

type registryEventPub struct {
	BrokerTag string `json:"broker_tag"`
}

// loadServiceDeps parses the embedded tacplus registry and derives per-service
// authorized_services from the declared dependency graph:
//   - boundary tags → which services host this service's state/config
//   - s2s client_dependencies → resolved through vservice host_tag to physical services
//   - event_publishes/event_watches → broker_tag identifies the event broker
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
	// host_tag and event_tag on vservice ServiceInfo entries.
	//   e.g. "agentsensor.StratumQuery" → "stratum"
	//        "agentsensor.AgentEventBusStateService" → "eventbus"
	grpcToPhysical := make(map[string]string)
	walkNodes(&root, func(node *registryNode) {
		if !strings.HasPrefix(node.FullPath, "vservices/") {
			return
		}
		for _, svc := range node.Services {
			if svc.HostTag != "" {
				grpcToPhysical[svc.GrpcSvc] = svc.HostTag
			}
			if svc.EventTag != "" {
				grpcToPhysical[svc.GrpcSvc] = svc.EventTag
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

		// Boundary tags → hosting service dependencies.
		for _, b := range node.Boundaries {
			if b.Tag != "" && isKernelAuthenticatedService(b.Tag) {
				deps[b.Tag] = true
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
			if ep.BrokerTag != "" && isKernelAuthenticatedService(ep.BrokerTag) {
				deps[ep.BrokerTag] = true
			}
		}

		// Event watches → broker service.
		for _, ew := range node.EventWatches {
			if ew.BrokerTag != "" && isKernelAuthenticatedService(ew.BrokerTag) {
				deps[ew.BrokerTag] = true
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
	// First: check if the target is a vservice service with a host_tag mapping.
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

func walkNodes(node *registryNode, fn func(*registryNode)) {
	fn(node)
	for _, child := range node.Children {
		walkNodes(child, fn)
	}
}
