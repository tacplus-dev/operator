package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	protoMailbox "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/mailbox"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// TestBuildKernelConfigSeedsMailboxConfigFromFile: services/mailbox.yaml is
// mailbox's typed Config and must reach kernel's services holder, or mailbox
// never learns which Workspace mailbox it acts as (gmail lives nowhere else).
func TestBuildKernelConfigSeedsMailboxConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &protoOperator.Config{Namespace: "workloads"}
	if err := SaveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	servicesDir := filepath.Join(dir, "services")
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(servicesDir, "kernel.yaml"), []byte("delegates: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mailboxYAML := []byte(`gmail:
  mailbox: hello@example.com
  send_as: crew@example.com
  send_as_name: Crew
  watch_topic: projects/p/topics/t
`)
	if err := os.WriteFile(filepath.Join(servicesDir, "mailbox.yaml"), mailboxYAML, 0644); err != nil {
		t.Fatal(err)
	}

	kcfg, err := BuildKernelConfigProto(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := kcfg.GetConfigs().GetMailboxConfig()
	if got == nil {
		t.Fatalf("services/mailbox.yaml did not reach the holder: Configs = %#v", kcfg.GetConfigs())
	}
	gm := got.GetGmail()
	if gm.GetMailbox() != "hello@example.com" || gm.GetSendAs() != "crew@example.com" ||
		gm.GetSendAsName() != "Crew" || gm.GetWatchTopic() != "projects/p/topics/t" {
		t.Fatalf("mailbox gmail = %+v, want the four fields from services/mailbox.yaml", gm)
	}
	if got.GetSweepIntervalSeconds() != int32(protoMailbox.DefaultSweepIntervalSeconds) {
		t.Fatalf("sweep_interval_seconds = %d, want default %d", got.GetSweepIntervalSeconds(), protoMailbox.DefaultSweepIntervalSeconds)
	}
}
