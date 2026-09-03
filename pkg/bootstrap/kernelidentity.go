package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	sm "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/s2sm"
	grpcconfig_pb "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/grpcconfig"
	"gitlab.com/secondstarcavalier/schematic/platform/services/operator/pkg/reconciler"
)

// startKernelIdentity brings up the operator's own ServiceManager and hands the
// resulting kernel client to the reconciler, so the reconcile loop can push the
// OPA config through kernel.Mint/UpdateOpaConfig.
//
// It runs in the background, and it must. sm.New() blocks until the operator
// can authenticate to kernel, retrying for many minutes before giving up — and
// on a fresh cluster kernel does not exist until this very reconciler has
// deployed it. Bootstrapping synchronously would deadlock the first install.
// Until it succeeds the reconciler simply has no pusher and skips the push; the
// change-detection in pushOpaConfig then sends the current config on the first
// reconcile after the client arrives.
//
// sm.New() is the same bootstrap letsencrypt and opa use. SetServing(true)
// follows it because New() leaves the health handle NOT_SERVING and only Run()
// flips it — and the operator does not call Run(): it serves no gRPC of its own
// here, it only needs the kernel identity and the S2S client that come with it.
func startKernelIdentity(cfgDir string, rec *reconciler.Reconciler, log *zap.Logger) {
	go func() {
		if err := writeOperatorBootstrapConfig(cfgDir); err != nil {
			log.Warn("operator kernel identity unavailable: writing bootstrap config",
				zap.Error(err))
			return
		}

		s, err := sm.New()
		if err != nil {
			log.Warn("operator kernel identity unavailable: opa config will not be pushed to kernel",
				zap.Error(err))
			return
		}
		s.SetServing(true)

		rec.SetOpaConfigPusher(s.Grpc().Clients().Services().MintClient())
		log.Info("operator kernel identity established")
	}()
}

// writeOperatorBootstrapConfig generates the operator's own chassis bootstrap
// config and points BOOTSTRAP_CONFIG at it, which is how grpcconfig.LoadConfig
// (and therefore sm.New) finds it.
//
// The operator generates this for itself because nothing else can: it is the
// component that generates every other service's bootstrap config, from the
// PKI and topology in its own config directory. The file is written to a temp
// dir rather than cfgDir so it is never picked up by the config bundle; it
// holds trusted CAs and endpoints only, no key material.
func writeOperatorBootstrapConfig(cfgDir string) error {
	dc, err := BuildDeployConfigs(cfgDir)
	if err != nil {
		return fmt.Errorf("building deploy configs: %w", err)
	}
	if len(dc.OperatorBootstrapConfig) == 0 {
		return fmt.Errorf("no operator bootstrap config was generated")
	}

	dir, err := os.MkdirTemp("", "operator-bootstrap")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	path := filepath.Join(dir, "bootstrap.yaml")
	if err := os.WriteFile(path, dc.OperatorBootstrapConfig, 0600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Setenv(grpcconfig_pb.BootstrapConfigEnv, path)
}
