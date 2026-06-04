package bootstrap

// Mint bootstrap-key model (INTEGRATION_PLAN.md §A0, KERNEL_TEARDOWN.md
// "BOOTSTRAP KEY rotation solution", settled 2026-06-03).
//
// mint is the sole signing authority but faces a cold-start cycle: to install
// its REAL public key into kernel's pub/sub trust set it must first authenticate
// to kernel, yet it has no trusted key until it does. The operator breaks the
// cycle: it provisions a per-mint BOOTSTRAP Ed25519 keypair and:
//
//   - emits the PRIVATE half into seed.yaml — a one-time bootstrap-secrets file,
//     distinct from durable operator.yaml, CONSUMED ONCE by mint then DISCARDED
//     (never persisted to durable config/state);
//   - seeds the PUBLIC half into kernel's trust set (JWKS / verifying-keys seed),
//     tagged bootstrap / short-TTL, so kernel trusts mint's first (bootstrap-
//     signed) S2S call when mint installs its real key.
//
// On startup mint loads the bootstrap key, generates its real keys, authenticates
// to kernel with the bootstrap key (ordinary JWT/JWKS S2S — the bootstrap pubkey
// is in the view), installs its real public key, then deletes the bootstrap key.
// The operator bootstrap key is cold-start ONLY; mint self-rotates real keys at
// runtime (no operator/redeploy involvement).

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	wrapperKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/libs/key"
	protoJwk "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/libs/jwk"
	protoKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/libs/key"
)

// bootstrapKeyTTL is the short lifetime advertised for an operator-seeded mint
// bootstrap key in kernel's trust set. The key authorizes only mint's first
// "install my real key" call, so it must expire quickly. Kept short so a leaked
// seed.yaml cannot be replayed once the cold start window passes.
//
// TODO(proto): kernel.Config has no JWKS/verifying-key seed field today (see
// BuildKernelBootstrapTrustEntries). When that field lands, decide whether the
// TTL is represented as an absolute `not_after` Unix timestamp or a relative
// `ttl_seconds`; this constant feeds whichever is chosen.
const bootstrapKeyTTL = 10 * time.Minute

// defaultMintReplicas is the number of mint instances provisioned with a
// bootstrap key when no explicit count is configured. mint runs as a small
// StatefulSet; each replica needs its own bootstrap key because only the first
// to come up installs the real key (the rest skip the bootstrap write — see the
// "skips the bootstrap write entirely if real mint keys already exist" rule).
const defaultMintReplicas = 1

// MintReplicaCount returns how many mint instances to provision bootstrap keys
// for. This is the CANONICAL count: one bootstrap key (and one per-ordinal seed
// Secret) is generated per replica. The helm generator's mintReplicas() reads
// the SAME CAVALIER_MINT_REPLICAS env var so the StatefulSet replica count and
// the number of provisioned seed entries stay in lock-step.
//
// TODO(proto): operator.Topology has no `mint_nodes` field (it is a generated
// proto type; adding the field is a proto-repo change owned by the lead). Until
// it lands, the count comes from CAVALIER_MINT_REPLICAS (default 1). When
// mint_nodes is added, read it here (and in helm.mintReplicas via the topology)
// and drop the env var.
func MintReplicaCount() int {
	if v := os.Getenv("CAVALIER_MINT_REPLICAS"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return defaultMintReplicas
}

// MintBootstrapKey is one provisioned per-mint bootstrap keypair. The public JWK
// is seeded into kernel's trust set; the private Key proto is delivered to that
// mint instance via seed.yaml and discarded after first use.
type MintBootstrapKey struct {
	// Instance is the mint instance label this key belongs to (e.g. "mint-0").
	Instance string
	// PublicJwk is the public half, formatted as a JWK for kernel's JWKS seed.
	PublicJwk *protoJwk.Jwk
	// PrivateKey is the private half, delivered to mint via seed.yaml.
	PrivateKey *protoKey.Key
}

// GenerateMintBootstrapKeys generates one Ed25519 bootstrap keypair per mint
// instance. Ed25519 matches mint's real signing keys and the existing cred-pair
// path (see GenerateCredPair).
func GenerateMintBootstrapKeys(count int) ([]MintBootstrapKey, error) {
	if count <= 0 {
		count = defaultMintReplicas
	}
	keys := make([]MintBootstrapKey, 0, count)
	for i := range count {
		instance := fmt.Sprintf("mint-%d", i)
		jwk, priv, err := generateBootstrapKeypair()
		if err != nil {
			return nil, fmt.Errorf("generating %s bootstrap key: %w", instance, err)
		}
		jwk.Use = protoJwk.JwkUseType_JWK_USE_TYPE_SIG
		keys = append(keys, MintBootstrapKey{
			Instance:   instance,
			PublicJwk:  jwk,
			PrivateKey: priv,
		})
	}
	return keys, nil
}

// generateBootstrapKeypair mirrors GenerateCredPair: an Ed25519 key whose public
// half is returned as a JWK and whose private Key proto is returned for seeding.
func generateBootstrapKeypair() (*protoJwk.Jwk, *protoKey.Key, error) {
	protoKeyMsg, err := wrapperKey.NewEd25519Key()
	if err != nil {
		return nil, nil, fmt.Errorf("generating Ed25519 key: %w", err)
	}
	protoKeyMsg.Created = time.Now().Unix()

	immKey, err := wrapperKey.NewImmutableKey(protoKeyMsg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating immutable key: %w", err)
	}

	pub := immKey.PrivateKey().(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	jwk := &protoJwk.Jwk{
		Kid:     protoKeyMsg.Id,
		Alg:     "EdDSA",
		Kty:     protoKey.KeyType_KEY_TYPE_OKP,
		Crv:     "Ed25519",
		X:       base64.RawURLEncoding.EncodeToString(pub),
		Use:     protoJwk.JwkUseType_JWK_USE_TYPE_SIG,
		Created: time.Now().Unix(),
	}
	return jwk, protoKeyMsg, nil
}

// SeedMintEntry is one mint instance's bootstrap secret in seed.yaml.
type SeedMintEntry struct {
	// Instance is the mint instance label (e.g. "mint-0").
	Instance string `yaml:"instance"`
	// Kid is the bootstrap key id; matches the public JWK seeded into kernel.
	Kid string `yaml:"kid"`
	// NotAfter is the Unix expiry of the bootstrap key's trust window. mint
	// must complete its real-key install (and discard this secret) before then.
	NotAfter int64 `yaml:"not_after"`
	// PrivateKey is the bootstrap private key (Ed25519), as a Key proto YAML map.
	PrivateKey map[string]any `yaml:"private_key"`
}

// Seed is the one-time bootstrap secrets artifact (seed.yaml). It is delivered to
// mint, CONSUMED ONCE, and then DISCARDED — it must NEVER be persisted to durable
// config/state. This is deliberately separate from operator.yaml (durable) and
// from crypto.yaml (durable PKI) so "delete the bootstrap key" is reinforced by
// the file lifecycle.
type Seed struct {
	// Version is the operator binary version that produced this seed.
	Version string `yaml:"version"`
	// BuiltAt is the Unix timestamp when the seed was generated.
	BuiltAt int64 `yaml:"built_at"`
	// MintBootstrap holds one entry per provisioned mint instance.
	MintBootstrap []SeedMintEntry `yaml:"mint_bootstrap"`
}

// BuildSeed converts provisioned bootstrap keys into the Seed (seed.yaml) holding
// the PRIVATE halves. notAfter is the absolute expiry stamped on every entry.
func BuildSeed(keys []MintBootstrapKey, notAfter int64) (*Seed, error) {
	entries := make([]SeedMintEntry, 0, len(keys))
	for _, k := range keys {
		keyYAML, err := protoKey.KeyToYAML(k.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("marshaling %s private key: %w", k.Instance, err)
		}
		var keyMap map[string]any
		if err := yaml.Unmarshal(keyYAML, &keyMap); err != nil {
			return nil, fmt.Errorf("unmarshaling %s private key YAML: %w", k.Instance, err)
		}
		entries = append(entries, SeedMintEntry{
			Instance:   k.Instance,
			Kid:        k.PublicJwk.Kid,
			NotAfter:   notAfter,
			PrivateKey: keyMap,
		})
	}
	return &Seed{
		Version:       Version,
		BuiltAt:       time.Now().Unix(),
		MintBootstrap: entries,
	}, nil
}

// SeedToYAML serializes a Seed to YAML for seed.yaml emission.
func SeedToYAML(s *Seed) ([]byte, error) {
	data, err := yaml.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("serializing seed: %w", err)
	}
	return data, nil
}

// PerInstanceSeedYAML builds one seed.yaml document per mint instance, keyed by
// instance label (e.g. "mint-0"). Each document contains ONLY that instance's
// bootstrap entry. mint runs as a StatefulSet with stable per-pod identity, so
// each replica must receive only its own discardable bootstrap private key (the
// dynamic-mode delivery mounts one document per pod via a per-ordinal Secret —
// see the helm generator's mint Secret template). notAfter is the absolute
// expiry stamped on every entry, matching the kernel trust seed.
func PerInstanceSeedYAML(keys []MintBootstrapKey, notAfter int64) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	for _, k := range keys {
		seed, err := BuildSeed([]MintBootstrapKey{k}, notAfter)
		if err != nil {
			return nil, fmt.Errorf("building %s seed: %w", k.Instance, err)
		}
		data, err := SeedToYAML(seed)
		if err != nil {
			return nil, fmt.Errorf("serializing %s seed: %w", k.Instance, err)
		}
		out[k.Instance] = data
	}
	return out, nil
}

// MintSeedHelmValues builds the "mint" Helm-value sub-map that delivers the
// one-time bootstrap seed to the mint StatefulSet. Each per-instance document is
// surfaced as seed<ordinal> (consumed by the per-ordinal mint seed Secret
// template); the combined seedYAML is also surfaced as "seed" for parity with
// the bundle/disk artifact. seedByInstance keys are "mint-<ordinal>".
//
// Returns nil when there is nothing to deliver.
func MintSeedHelmValues(seedYAML []byte, seedByInstance map[string][]byte) map[string]interface{} {
	mintVals := map[string]interface{}{}
	if len(seedYAML) > 0 {
		mintVals["seed"] = string(seedYAML)
	}
	for instance, data := range seedByInstance {
		if ord, ok := strings.CutPrefix(instance, "mint-"); ok {
			mintVals["seed"+ord] = string(data)
		}
	}
	if len(mintVals) == 0 {
		return nil
	}
	return mintVals
}

// SplitSeedYAML parses a combined seed.yaml (all mint instances) back into
// per-instance single-entry documents keyed by instance label ("mint-0", …).
// Used by the bundle apply path, where only the combined seed is stored, to
// reconstruct the per-ordinal Secret values. Returns nil for empty input.
func SplitSeedYAML(seedYAML []byte) (map[string][]byte, error) {
	if len(seedYAML) == 0 {
		return nil, nil
	}
	var s Seed
	if err := yaml.Unmarshal(seedYAML, &s); err != nil {
		return nil, fmt.Errorf("parsing combined seed: %w", err)
	}
	out := make(map[string][]byte, len(s.MintBootstrap))
	for _, entry := range s.MintBootstrap {
		single := Seed{Version: s.Version, BuiltAt: s.BuiltAt, MintBootstrap: []SeedMintEntry{entry}}
		data, err := SeedToYAML(&single)
		if err != nil {
			return nil, fmt.Errorf("serializing %s seed: %w", entry.Instance, err)
		}
		out[entry.Instance] = data
	}
	return out, nil
}

// KernelBootstrapTrustEntry is one public bootstrap key seeded into kernel's
// trust set, tagged bootstrap / short-TTL. Kernel trusts JWTs signed by this key
// only until NotAfter, and only for mint's first real-key install.
type KernelBootstrapTrustEntry struct {
	// Jwk is the public bootstrap key.
	Jwk map[string]any `yaml:"jwk"`
	// Bootstrap marks this as an operator-seeded cold-start key (not a durable
	// trust anchor), so kernel/tooling can purge it once consumed.
	Bootstrap bool `yaml:"bootstrap"`
	// NotAfter is the Unix expiry of the trust window.
	NotAfter int64 `yaml:"not_after"`
}

// BuildKernelBootstrapTrustEntries renders the PUBLIC halves into kernel-trust
// entries (the JWKS / JwkVerifyingKeysView seed), tagged bootstrap / short-TTL.
//
// TODO(proto): kernel.Config has NO field to carry a JWKS/verifying-key seed
// today (it has cluster_ca, trusted_cas, cluster_cred_pairs — none model a
// bootstrap JWK with a TTL). These entries therefore cannot yet be injected into
// the kernel Config proto. Until a field exists (e.g. kernel.Config
// `repeated BootstrapJwk bootstrap_verifying_keys` with a not_after), the
// operator can only EMIT these alongside the bundle for kernel to ingest, OR a
// proto change must add the seed field and DeployConfigs must populate it in
// BuildDeployConfigs (next to ClusterCredPairs / TrustedCas). See
// operator_mint_seed.md "Open questions".
func BuildKernelBootstrapTrustEntries(keys []MintBootstrapKey, notAfter int64) ([]KernelBootstrapTrustEntry, error) {
	entries := make([]KernelBootstrapTrustEntry, 0, len(keys))
	for _, k := range keys {
		jwkYAML, err := protoJwk.JwkToYAML(k.PublicJwk)
		if err != nil {
			return nil, fmt.Errorf("marshaling %s public JWK: %w", k.Instance, err)
		}
		var jwkMap map[string]any
		if err := yaml.Unmarshal(jwkYAML, &jwkMap); err != nil {
			return nil, fmt.Errorf("unmarshaling %s public JWK YAML: %w", k.Instance, err)
		}
		entries = append(entries, KernelBootstrapTrustEntry{
			Jwk:       jwkMap,
			Bootstrap: true,
			NotAfter:  notAfter,
		})
	}
	return entries, nil
}

// BootstrapKeyExpiry returns the absolute Unix expiry for bootstrap keys
// generated now (now + bootstrapKeyTTL).
func BootstrapKeyExpiry() int64 {
	return time.Now().Add(bootstrapKeyTTL).Unix()
}
