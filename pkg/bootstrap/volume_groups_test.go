package bootstrap

import (
	"strings"
	"testing"

	protoBlobstore "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/blobstore"
)

// volumeGroupRegistry is a registry.json shaped exactly like the one tacplus
// emits for the fields this walk reads: a services/ node per service, each with
// a ServiceInfo carrying volume_groups. The 64-bit sizes are quoted because
// protojson quotes them, which is what the real registry.json carries.
const volumeGroupRegistry = `{
  "name": "root",
  "children": [
    {
      "name": "docmanager",
      "full_path": "services/docmanager",
      "services": [
        {
          "name": "DocManager",
          "package": "docmanager",
          "volume_groups": [
            {
              "name": "documents",
              "max_blob_bytes": "52428800",
              "content_types": ["application/pdf", "text/plain"],
              "tenant_quota_bytes": "53687091200"
            }
          ]
        }
      ]
    },
    {
      "name": "gallery",
      "full_path": "services/gallery",
      "services": [
        {
          "name": "Gallery",
          "package": "gallery",
          "volume_groups": [
            {
              "name": "photos",
              "max_blob_bytes": 1048576,
              "content_types": ["image/jpeg"],
              "tenant_quota_bytes": 2097152
            },
            {
              "name": "thumbnails",
              "max_blob_bytes": 4096,
              "content_types": ["image/png"],
              "tenant_quota_bytes": 8192
            }
          ]
        }
      ]
    }
  ]
}`

// TestVolumeGroupsFromTwoServices is the rendering half of the blob store's
// work split: two services declare groups, and every declared group reaches
// blobstore's config with its rules intact.
func TestVolumeGroupsFromTwoServices(t *testing.T) {
	cfg := &protoBlobstore.Config{}
	if err := setBlobstoreVolumeGroups(cfg, []byte(volumeGroupRegistry)); err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, g := range cfg.GetVolumeGroups() {
		names = append(names, g.GetName())
	}
	want := []string{"documents", "photos", "thumbnails"}
	if len(names) != len(want) {
		t.Fatalf("rendered groups %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("rendered groups %v, want %v (sorted by name)", names, want)
		}
	}

	documents := cfg.GetVolumeGroups()[0]
	if documents.GetMaxBlobBytes() != 52428800 {
		t.Errorf("documents max_blob_bytes = %d, want 52428800 (quoted uint64 in the registry)", documents.GetMaxBlobBytes())
	}
	if documents.GetTenantQuotaBytes() != 53687091200 {
		t.Errorf("documents tenant_quota_bytes = %d, want 53687091200", documents.GetTenantQuotaBytes())
	}
	if got := documents.GetContentTypes(); len(got) != 2 || got[0] != "application/pdf" || got[1] != "text/plain" {
		t.Errorf("documents content_types = %v, want [application/pdf text/plain]", got)
	}
	if photos := cfg.GetVolumeGroups()[1]; photos.GetMaxBlobBytes() != 1048576 {
		t.Errorf("photos max_blob_bytes = %d, want 1048576 (bare JSON number in the registry)", photos.GetMaxBlobBytes())
	}
}

// TestVolumeGroupsRejectDuplicateName pins the fail-loud half: a group name is
// the first path segment of every file in the group, so two services claiming
// one name must not silently dedupe or let the last writer win.
func TestVolumeGroupsRejectDuplicateName(t *testing.T) {
	const dup = `{
  "name": "root",
  "children": [
    {
      "name": "docmanager",
      "full_path": "services/docmanager",
      "services": [
        {"name": "DocManager", "volume_groups": [
          {"name": "documents", "max_blob_bytes": "1", "content_types": ["text/plain"], "tenant_quota_bytes": "2"}
        ]}
      ]
    },
    {
      "name": "archiver",
      "full_path": "services/archiver",
      "services": [
        {"name": "Archiver", "volume_groups": [
          {"name": "documents", "max_blob_bytes": "3", "content_types": ["application/pdf"], "tenant_quota_bytes": "4"}
        ]}
      ]
    }
  ]
}`

	cfg := &protoBlobstore.Config{}
	err := setBlobstoreVolumeGroups(cfg, []byte(dup))
	if err == nil {
		t.Fatalf("duplicate group name accepted, rendered %d group(s)", len(cfg.GetVolumeGroups()))
	}
	for _, want := range []string{"documents", "docmanager", "archiver"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestVolumeGroupsSameServiceTwiceIsNotADuplicate covers the shape tacplus
// actually emits for a service-level annotation: it is stamped onto every
// ServiceInfo one proto file produces, so a split c2s/s2s service repeats its
// own group. That is duplication in the registry, not a name clash.
func TestVolumeGroupsSameServiceTwiceIsNotADuplicate(t *testing.T) {
	const twice = `{
  "name": "root",
  "children": [
    {
      "name": "docmanager",
      "full_path": "services/docmanager",
      "services": [
        {"name": "DocManager", "volume_groups": [
          {"name": "documents", "max_blob_bytes": "1", "content_types": ["text/plain"], "tenant_quota_bytes": "2"}
        ]},
        {"name": "DocManagerInternal", "volume_groups": [
          {"name": "documents", "max_blob_bytes": "1", "content_types": ["text/plain"], "tenant_quota_bytes": "2"}
        ]}
      ]
    }
  ]
}`

	cfg := &protoBlobstore.Config{}
	if err := setBlobstoreVolumeGroups(cfg, []byte(twice)); err != nil {
		t.Fatal(err)
	}
	if len(cfg.GetVolumeGroups()) != 1 {
		t.Fatalf("rendered %d groups, want 1", len(cfg.GetVolumeGroups()))
	}
}

// TestVolumeGroupsEmptyRegistryStaysEmpty is the no-regression case: a fleet
// whose services declare no groups gets an empty list, which is blobstore's
// correct fail-closed state, not an error.
//
// It uses a fixed registry rather than the embedded one: the embedded registry
// is whatever fleet the operator was generated against, and docmanager (in the
// sonar fleet) declares a "documents" group.
func TestVolumeGroupsEmptyRegistryStaysEmpty(t *testing.T) {
	const noGroups = `{
  "name": "root",
  "children": [
    {
      "name": "gateway",
      "full_path": "services/gateway",
      "services": [{"name": "Gateway", "package": "gateway"}]
    }
  ]
}`
	cfg := &protoBlobstore.Config{}
	if err := setBlobstoreVolumeGroups(cfg, []byte(noGroups)); err != nil {
		t.Fatal(err)
	}
	if len(cfg.GetVolumeGroups()) != 0 {
		t.Fatalf("rendered %d group(s), want 0", len(cfg.GetVolumeGroups()))
	}
}
