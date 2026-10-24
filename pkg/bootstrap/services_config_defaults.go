package bootstrap

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	protoServicesConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/services_config"
)

// Kernel re-seeds every service's ConfigState root from its services holder on
// every boot (decisions/2026-09-23-kernel-reseeds-every-config-root-from-the-bundle.md),
// so an entry the bundle does not mention is pushed as an all-zero message, and
// any field carrying an assert_ge floor above zero then fails kernel's own
// validation forever — the service crash-loops at boot with "field X must be
// >= N, got: 0" (mailbox, messaging and weather all did).
//
// tacplus generates ServicesDefaults for the holder, which applies each PRESENT
// entry's generated ConfigDefaults. So the operator's job is only to make the
// deployed services' entries present; the generated code does the rest, for
// whatever services the fleet is built from. (This used to be a hand-written
// table naming every service's package — the whole-fleet assumption the fleet
// build removes.)

// servicesHolderDefaultsSkipped are holder entries the operator deliberately
// leaves absent even when the service is deployed, because their Config carries
// an assert_required field that has no default at all: a defaulted-but-empty
// entry fails the operator's own output validation (ValidateDeployConfigs),
// which would block generation for every cluster running that service.
//
//   - letsencrypt: hostnames is assert_required; the operator delivers
//     letsencrypt's config out of band as DeployConfigs.LetsencryptServiceConfig.
//
// It therefore still gets whatever kernel seeds from an absent holder entry.
var servicesHolderDefaultsSkipped = map[string]bool{
	"letsencrypt": true,
}

// ApplyServicesHolderDefaults populates the holder entry of every deployed
// service and applies the generated defaults, so kernel never re-seeds a
// deployed service's ConfigState from an all-zero config. A service with no
// holder entry is skipped: the fleet it is built for does not carry it.
func ApplyServicesHolderDefaults(s *protoServicesConfig.Services, services []string) error {
	if s == nil {
		return nil
	}
	holder := s.ProtoReflect()
	for _, name := range services {
		if servicesHolderDefaultsSkipped[name] {
			continue
		}
		fd := holder.Descriptor().Fields().ByName(protoreflect.Name(name + "_config"))
		if fd == nil || fd.Message() == nil {
			continue
		}
		holder.Mutable(fd) // present, so the generated defaults reach it
	}
	protoServicesConfig.ServicesDefaults(s)
	return nil
}
