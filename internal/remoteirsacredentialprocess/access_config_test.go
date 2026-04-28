package remoteirsacredentialprocess

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
)

func TestBuildAccessConfigRejectsEmptyProviderFile(t *testing.T) {
	_, err := BuildAccessConfig(AccessConfigOptions{})
	if err == nil {
		t.Fatal("BuildAccessConfig returned nil error")
	}

	if err.Error() != "clusterprofile provider file is required" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestBuildAccessConfigWrapsProviderFileLoadError(t *testing.T) {
	_, err := BuildAccessConfig(AccessConfigOptions{
		ProviderFile: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil {
		t.Fatal("BuildAccessConfig returned nil error")
	}

	if !strings.Contains(err.Error(), "load clusterprofile provider file:") {
		t.Fatalf("error = %q, want prefix containing load clusterprofile provider file:", err.Error())
	}
}

func TestBuildAccessConfigLoadsProviderFile(t *testing.T) {
	providerFile := filepath.Join(t.TempDir(), "providers.json")
	writeTestProviderFile(t, providerFile)

	cfg, err := BuildAccessConfig(AccessConfigOptions{
		ProviderFile: providerFile,
	})
	if err != nil {
		t.Fatalf("BuildAccessConfig returned error: %v", err)
	}

	assertTestAccessConfig(t, cfg)
}

func writeTestProviderFile(t *testing.T, providerFile string) {
	t.Helper()

	config := access.Config{
		Providers: []access.Provider{{
			Name: "test-provider",
			ExecConfig: &clientcmdapi.ExecConfig{
				APIVersion:         "client.authentication.k8s.io/v1",
				Command:            "/plugins/test-provider",
				Args:               []string{"--flag=value"},
				ProvideClusterInfo: true,
				InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			},
		}},
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal access config: %v", err)
	}

	if err := os.WriteFile(providerFile, data, 0o600); err != nil {
		t.Fatalf("write access config: %v", err)
	}
}

func assertTestAccessConfig(t *testing.T, cfg *access.Config) {
	t.Helper()

	if len(cfg.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(cfg.Providers))
	}

	provider := cfg.Providers[0]
	if provider.Name != "test-provider" {
		t.Fatalf("provider name = %q", provider.Name)
	}

	if provider.ExecConfig == nil {
		t.Fatal("ExecConfig is nil")
	}

	if provider.ExecConfig.APIVersion != "client.authentication.k8s.io/v1" {
		t.Fatalf("apiVersion = %q", provider.ExecConfig.APIVersion)
	}

	if provider.ExecConfig.Command != "/plugins/test-provider" {
		t.Fatalf("command = %q", provider.ExecConfig.Command)
	}

	if got, want := provider.ExecConfig.Args, []string{"--flag=value"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("args = %#v, want %#v", got, want)
	}

	if !provider.ExecConfig.ProvideClusterInfo {
		t.Fatal("ProvideClusterInfo = false, want true")
	}

	if provider.ExecConfig.InteractiveMode != clientcmdapi.NeverExecInteractiveMode {
		t.Fatalf("interactiveMode = %q", provider.ExecConfig.InteractiveMode)
	}
}
