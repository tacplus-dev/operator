package bootstrap

import (
	"fmt"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/cli"

	loggerchassis "gitlab.com/seabeam/schematic/platform/services/operator/model/chassis/logger"
	"gitlab.com/seabeam/schematic/platform/services/operator/pkg/reconciler"
)

var recoverReleaseNamespace string

var recoverReleaseCmd = &cobra.Command{
	Use:   "recover-release <name>",
	Short: "Roll back a Helm release left stale in a pending state",
	Long: `Recovers a Helm release in the current Kubernetes context whose latest
revision has been pending (pending-upgrade, pending-install, pending-rollback)
for longer than the reconciler's stale age, using the reconciler's own recovery:
roll back to the last deployed revision, or the newest superseded/failed one
before it, or uninstall an interrupted first install.

A helm CLI that dies without a catchable signal leaves such a record behind,
and every later upgrade of the release refuses with "another operation is in
progress". Run this before upgrading a release with the helm CLI. A release
that is absent or not pending is left alone; one pending for less than the
stale age is an error, because a helm process may still own it.

Example:
  schematic-operator recover-release schematic-operator -n schematic`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		log, err := loggerchassis.InstallFromConfig(nil)
		if err != nil {
			return fmt.Errorf("init logger: %w", err)
		}
		defer log.Sync() //nolint:errcheck

		settings := cli.New()
		settings.SetNamespace(recoverReleaseNamespace)
		cfg := new(action.Configuration)
		if err := cfg.Init(settings.RESTClientGetter(), recoverReleaseNamespace, "secret", func(string, ...interface{}) {}); err != nil {
			return fmt.Errorf("initialising helm action config: %w", err)
		}
		recovered, err := reconciler.RecoverStalePendingRelease(cfg, args[0], log)
		if err != nil {
			return err
		}
		if recovered {
			fmt.Printf("Release %s recovered from a stale pending revision.\n", args[0])
		}
		return nil
	},
}

func init() {
	recoverReleaseCmd.Flags().StringVarP(&recoverReleaseNamespace, "namespace", "n", "schematic", "namespace holding the release")
}
