// compose_guard_test.go: CIC-170 regression.
//
// After CIC-150, the server's apiStartupGuard refuses to start when
// neither CICADO_API_TOKEN nor CICADO_INSECURE_API is set.  The
// docker-compose.yml dev stack has no real secrets, so it must
// set CICADO_INSECURE_API.  This test guards against the env var
// being accidentally removed or the YAML becoming invalid.
package main

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeSpec is the minimal struct needed by this test to extract
// the server service's environment from docker-compose.yml.
type composeSpec struct {
	Services struct {
		Server struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"server"`
	}
}

// TestComposeInsecureAPISet ensures docker-compose.yml sets
// CICADO_INSECURE_API: "true" for the server service, and that the
// file is valid YAML.  If the file is missing or the key is absent,
// the test fails.
//
// go test CWD is the package directory; docker-compose.yml is at
// the module root (3 levels up from server/cmd/cicado-server/).
func TestComposeInsecureAPISet(t *testing.T) {
	content, err := os.ReadFile("../../../docker-compose.yml")
	if err != nil {
		t.Fatalf("docker-compose.yml not found: %v", err)
	}

	var spec composeSpec
	if err := yaml.Unmarshal(content, &spec); err != nil {
		t.Errorf("docker-compose.yml is not valid YAML: %v", err)
		return
	}

	env := spec.Services.Server.Environment
	if env == nil {
		t.Error("docker-compose.yml: server service has no 'environment' map")
		return
	}
	if got := env["CICADO_INSECURE_API"]; got != "true" {
		t.Errorf("CICADO_INSECURE_API = %q, want \"true\"; "+
			"make dev would die at startup after CIC-150's apiStartupGuard",
			got)
	}
	// Also assert CICADO_INSECURE_WEBHOOK (same fix, same test).
	if got := env["CICADO_INSECURE_WEBHOOK"]; got != "true" {
		t.Errorf("CICADO_INSECURE_WEBHOOK = %q, want \"true\"", got)
	}
}
