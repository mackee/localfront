//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// runExampleScenario verifies one examples/<dir> end to end with the runn CLI
// (installed via aqua): it starts an echo origin and localfront serving the
// actual example template, then runs the example's scenario.yaml against the
// data plane. It skips when runn is not on PATH.
func runExampleScenario(t *testing.T, exampleDir string, extraServeArgs ...string) {
	t.Helper()
	runnBin, err := exec.LookPath("runn")
	if err != nil {
		t.Skip("runn not installed (run `aqua i`); skipping example scenario verification")
	}

	host, port := echoOrigin(t)

	raw, err := os.ReadFile(repoFile(t, exampleDir+"/template.yaml"))
	if err != nil {
		t.Fatalf("read example template: %v", err)
	}
	// Point the example's origin at the echo backend. Only the environment-
	// specific port (and loopback host) change; the template's structure,
	// functions, and policies are what we verify.
	tmpl := strings.ReplaceAll(string(raw), "HTTPPort: 3000", "HTTPPort: "+strconv.Itoa(port))
	if host != "127.0.0.1" {
		tmpl = strings.ReplaceAll(tmpl, "127.0.0.1", host)
	}
	templatePath := writeFile(t, "template.yaml", tmpl)

	args := append([]string{"serve", "--template", templatePath, "--listen", "127.0.0.1:0"}, extraServeArgs...)
	lf := startLocalfrontArgs(t, args...)

	// Run from the book's directory so runn does not need the read:parent scope.
	cmd := exec.Command(runnBin, "run", "scenario.yaml")
	cmd.Dir = repoFile(t, exampleDir)
	cmd.Env = append(os.Environ(), "LF_ENDPOINT="+lf.baseURL)
	out, err := cmd.CombinedOutput()
	t.Logf("runn output:\n%s", out)
	if err != nil {
		t.Fatalf("runn run failed: %v", err)
	}
}

// Scenario 3: CloudFront Functions — URL normalization, redirect, KVS flag.
func TestExampleFunctions_Runn(t *testing.T) {
	runExampleScenario(t, "examples/functions",
		"--kvs-seed", "feature-flags="+repoFile(t, "examples/functions/flags.json"),
	)
}

// Scenario 6: CORS preflight + security headers via a response headers policy.
func TestExampleCORS_Runn(t *testing.T) {
	runExampleScenario(t, "examples/cors-security")
}
