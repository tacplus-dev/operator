package bootstrap

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	protoServicesConfig "gitlab.com/seabeam/schematic/platform/services/operator/model/types/generated/services_config"
	protoBeagle "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/beagle"
	protoBlobstore "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/blobstore"
	protoDocmanager "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/docmanager"
	protoEventbus "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/eventbus"
	protoGateway "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/gateway"
	protoKmaker "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kmaker"
	protoLetsencrypt "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/letsencrypt"
	protoLlmproxy "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/llmproxy"
	protoLlmsettings "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/llmsettings"
	protoMailbox "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/mailbox"
	protoMessaging "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/messaging"
	protoOauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/oauth"
	protoOpa "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/opa"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
	protoOtto "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/otto"
	protoPersonalauth "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/personalauth"
	protoProfile "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/profile"
	protoReveille "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/reveille"
	protoScotty "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/scotty"
	protoScottyagent "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/scottyagent"
	protoScottypersonal "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/scottypersonal"
	protoStratum "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/stratum"
	protoTally "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/tally"
	protoWeather "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/weather"
	protoWork "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/work"
)

// servicesHolderDefaulters routes each entry of kernel's services holder to its
// service's generated ConfigDefaults, keyed by the holder's proto field name.
//
// tacplus generates a ConfigDefaults for every service's typed Config, but the
// generated ServicesDefaults for the holder itself is a no-op: it does not
// recurse into its message-typed fields. Kernel re-seeds every service's
// ConfigState root from this holder on every boot (see
// decisions/2026-09-23-kernel-reseeds-every-config-root-from-the-bundle.md), so
// an entry the bundle does not mention is pushed as an all-zero message, and any
// field carrying an assert_ge floor above zero then fails kernel's own
// validation forever — the service crash-loops at boot with "field X must be
// >= N, got: 0" (mailbox, messaging and weather all did). Gateway only escaped
// that by being special-cased by hand in deploy.go.
var servicesHolderDefaulters = map[string]func(proto.Message){
	"gateway_config":        func(m proto.Message) { protoGateway.ConfigDefaults(m.(*protoGateway.Config)) },
	"letsencrypt_config":    func(m proto.Message) { protoLetsencrypt.ConfigDefaults(m.(*protoLetsencrypt.Config)) },
	"eventbus_config":       func(m proto.Message) { protoEventbus.ConfigDefaults(m.(*protoEventbus.Config)) },
	"oauth_config":          func(m proto.Message) { protoOauth.ConfigDefaults(m.(*protoOauth.Config)) },
	"opa_config":            func(m proto.Message) { protoOpa.ConfigDefaults(m.(*protoOpa.Config)) },
	"stratum_config":        func(m proto.Message) { protoStratum.ConfigDefaults(m.(*protoStratum.Config)) },
	"operator_config":       func(m proto.Message) { protoOperator.ConfigDefaults(m.(*protoOperator.Config)) },
	"kmaker_config":         func(m proto.Message) { protoKmaker.ConfigDefaults(m.(*protoKmaker.Config)) },
	"messaging_config":      func(m proto.Message) { protoMessaging.ConfigDefaults(m.(*protoMessaging.Config)) },
	"scottyagent_config":    func(m proto.Message) { protoScottyagent.ConfigDefaults(m.(*protoScottyagent.Config)) },
	"mailbox_config":        func(m proto.Message) { protoMailbox.ConfigDefaults(m.(*protoMailbox.Config)) },
	"llmsettings_config":    func(m proto.Message) { protoLlmsettings.ConfigDefaults(m.(*protoLlmsettings.Config)) },
	"personalauth_config":   func(m proto.Message) { protoPersonalauth.ConfigDefaults(m.(*protoPersonalauth.Config)) },
	"scotty_config":         func(m proto.Message) { protoScotty.ConfigDefaults(m.(*protoScotty.Config)) },
	"scottypersonal_config": func(m proto.Message) { protoScottypersonal.ConfigDefaults(m.(*protoScottypersonal.Config)) },
	"work_config":           func(m proto.Message) { protoWork.ConfigDefaults(m.(*protoWork.Config)) },
	"weather_config":        func(m proto.Message) { protoWeather.ConfigDefaults(m.(*protoWeather.Config)) },
	"llmproxy_config":       func(m proto.Message) { protoLlmproxy.ConfigDefaults(m.(*protoLlmproxy.Config)) },
	"profile_config":        func(m proto.Message) { protoProfile.ConfigDefaults(m.(*protoProfile.Config)) },
	"blobstore_config":      func(m proto.Message) { protoBlobstore.ConfigDefaults(m.(*protoBlobstore.Config)) },
	"docmanager_config":     func(m proto.Message) { protoDocmanager.ConfigDefaults(m.(*protoDocmanager.Config)) },
	"reveille_config":       func(m proto.Message) { protoReveille.ConfigDefaults(m.(*protoReveille.Config)) },
	"beagle_config":         func(m proto.Message) { protoBeagle.ConfigDefaults(m.(*protoBeagle.Config)) },
	"otto_config":           func(m proto.Message) { protoOtto.ConfigDefaults(m.(*protoOtto.Config)) },
	"tally_config":          func(m proto.Message) { protoTally.ConfigDefaults(m.(*protoTally.Config)) },
}

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
// Closing that needs a real bundle section (or an operator-computed value), not
// a default — tracked as follow-up, not fixed here.
var servicesHolderDefaultsSkipped = map[string]bool{
	"letsencrypt": true,
}

// ApplyServicesHolderDefaults populates the holder entry of every deployed
// service and applies that service's generated ConfigDefaults to it, so kernel
// never re-seeds a deployed service's ConfigState from an all-zero config. A
// service with no holder entry is skipped; a holder entry with no defaulter is
// refused, because leaving it silently zero is the bug this closes.
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
		defaults, ok := servicesHolderDefaulters[string(fd.Name())]
		if !ok {
			return fmt.Errorf("kernel's services holder declares %s, but servicesHolderDefaulters has no entry for it — add one so kernel does not seed %s an all-zero config", fd.Name(), name)
		}
		defaults(holder.Mutable(fd).Message().Interface())
	}
	return nil
}
