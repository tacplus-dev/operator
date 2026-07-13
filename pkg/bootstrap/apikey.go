package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	protoJwk "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/jwk"
	protoKey "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/sys/key"
)

var (
	apiKeyProfiles []string
	apiKeyClaims   []string
)

// AddApiKey generates a cred pair for a named API key, writes it to disk,
// adds the api_key entry to services/oauth.yaml, and registers a kernel
// delegate so the API key holder can access backend services.
func AddApiKey(cfgDir, name string, profiles []string, claims map[string]string) error {
	credsDir := filepath.Join(cfgDir, "creds")
	credDir := filepath.Join(credsDir, name)
	oauthPath := filepath.Join(cfgDir, "services", "oauth.yaml")
	kernelPath := filepath.Join(cfgDir, "services", "kernel.yaml")

	if _, err := os.Stat(oauthPath); os.IsNotExist(err) {
		return fmt.Errorf("no services/oauth.yaml found — run 'cavalier-operator init cluster' first")
	}

	for _, p := range profiles {
		if err := validateProfile(kernelPath, p); err != nil {
			return err
		}
	}

	jwk, privateKey, err := GenerateCredPair()
	if err != nil {
		return fmt.Errorf("generating cred pair: %w", err)
	}

	if err := os.MkdirAll(credDir, 0755); err != nil {
		return fmt.Errorf("creating cred directory: %w", err)
	}

	credBytes, err := protoJwk.JwkToYAML(jwk)
	if err != nil {
		return fmt.Errorf("marshaling cred pair: %w", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "creds.yaml"), credBytes, 0644); err != nil {
		return fmt.Errorf("writing creds: %w", err)
	}

	keyBytes, err := protoKey.KeyToYAML(privateKey)
	if err != nil {
		return fmt.Errorf("marshaling private key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "key.yaml"), keyBytes, 0600); err != nil {
		return fmt.Errorf("writing key: %w", err)
	}

	if err := addApiKeyToOauthConfig(oauthPath, name, profiles, jwk); err != nil {
		return fmt.Errorf("updating oauth config: %w", err)
	}

	if _, err := os.Stat(kernelPath); err == nil {
		if err := AddDelegate(kernelPath, name, "oauth", claims); err != nil {
			return fmt.Errorf("adding kernel delegate: %w", err)
		}
	}

	return nil
}

func validateProfile(kernelPath, profile string) error {
	data, err := os.ReadFile(kernelPath)
	if err != nil {
		return fmt.Errorf("reading kernel config: %w", err)
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing kernel config: %w", err)
	}

	var validIDs []string
	if profiles, ok := cfg["entitlement_profiles"]; ok {
		if list, ok := profiles.([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if id, ok := m["id"].(string); ok {
						if id == profile {
							return nil
						}
						validIDs = append(validIDs, id)
					}
				}
			}
		}
	}

	return fmt.Errorf("profile %q not found\n\nAvailable profiles: %s", profile, strings.Join(validIDs, ", "))
}

func addApiKeyToOauthConfig(oauthPath, name string, profiles []string, jwk *protoJwk.Jwk) error {
	data, err := os.ReadFile(oauthPath)
	if err != nil {
		return fmt.Errorf("reading oauth config: %w", err)
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing oauth config: %w", err)
	}

	jwkJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(jwk)
	if err != nil {
		return fmt.Errorf("marshaling JWK: %w", err)
	}
	var jwkMap map[string]any
	if err := yaml.Unmarshal(jwkJSON, &jwkMap); err != nil {
		return fmt.Errorf("parsing JWK JSON as map: %w", err)
	}

	profileList := make([]any, len(profiles))
	for i, p := range profiles {
		profileList[i] = p
	}
	apiKeyEntry := map[string]any{
		"name":       name,
		"profiles":   profileList,
		"cred_pairs": []any{jwkMap},
	}

	var apiKeys []any
	if existing, ok := cfg["api_keys"]; ok {
		if list, ok := existing.([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					if m["name"] == name {
						return fmt.Errorf("api_key %q already exists in oauth config", name)
					}
				}
			}
			apiKeys = list
		}
	}
	apiKeys = append(apiKeys, apiKeyEntry)
	cfg["api_keys"] = apiKeys

	outData, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling oauth config: %w", err)
	}

	return os.WriteFile(oauthPath, outData, 0644)
}
