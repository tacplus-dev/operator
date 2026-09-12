package bootstrap

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// clearBackendTokenEnv unsets every provider variable so a test only sees the ones it sets.
func clearBackendTokenEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{"ANTHROPIC_TOKEN", "GITLAB_TOKEN", "OPENAI_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(env, "")
	}
}

// The command's help text documents four provider variables; each one must actually
// reach the generated policy.
func TestWriteBackendTokenPolicyReadsDocumentedEnvVars(t *testing.T) {
	for _, tc := range []struct {
		env, provider, token string
	}{
		{"ANTHROPIC_TOKEN", "anthropic", "sk-ant-test"},
		{"GITLAB_TOKEN", "gitlab", "glpat-test"},
		{"OPENAI_TOKEN", "openai", "sk-openai-test"},
		{"GITHUB_TOKEN", "github", "ghp-test"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			clearBackendTokenEnv(t)
			t.Setenv(tc.env, tc.token)

			regoPath, names, err := writeBackendTokenPolicy(t.TempDir())
			if err != nil {
				t.Fatalf("writing backend token policy: %v", err)
			}
			if !reflect.DeepEqual(names, []string{tc.provider}) {
				t.Fatalf("providers = %v, want [%s]", names, tc.provider)
			}

			rego, err := os.ReadFile(regoPath)
			if err != nil {
				t.Fatalf("reading generated policy: %v", err)
			}
			if !strings.Contains(string(rego), `"`+tc.provider+`": "`+tc.token+`",`) {
				t.Fatalf("generated policy does not map %s to its token:\n%s", tc.provider, rego)
			}
		})
	}
}

func TestWriteBackendTokenPolicyIncludesEveryProviderSet(t *testing.T) {
	clearBackendTokenEnv(t)
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-test")
	t.Setenv("GITLAB_TOKEN", "glpat-test")
	t.Setenv("OPENAI_TOKEN", "sk-openai-test")
	t.Setenv("GITHUB_TOKEN", "ghp-test")

	_, names, err := writeBackendTokenPolicy(t.TempDir())
	if err != nil {
		t.Fatalf("writing backend token policy: %v", err)
	}
	want := []string{"anthropic", "gitlab", "openai", "github"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("providers = %v, want %v", names, want)
	}
}

func TestWriteBackendTokenPolicyWithoutAnyToken(t *testing.T) {
	clearBackendTokenEnv(t)
	if _, _, err := writeBackendTokenPolicy(t.TempDir()); err == nil {
		t.Fatal("expected an error when no provider token is set")
	}
}
