package testimage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Compose files retain literal defaults so each example works on its own.
// Catch drift in the ordinary Go test job when the RustFS pin changes.
func TestRustFSReferencesStayAligned(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*/compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example Compose files found")
	}
	matched := 0
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		contents := string(body)
		if !strings.Contains(contents, "rustfs/rustfs:") && !strings.Contains(contents, "LOCALFRONT_S3_IMAGE") {
			continue
		}
		matched++
		if !strings.Contains(contents, "${LOCALFRONT_S3_IMAGE:-"+RustFS+"}") {
			t.Errorf("%s does not use the pinned RustFS default", path)
		}
		if strings.Contains(contents, "LOCALFRONT_E2E_S3_IMAGE") {
			t.Errorf("%s reads the Go E2E image override", path)
		}
		if !strings.Contains(contents, "http://localhost:9000/health") || !strings.Contains(contents, "condition: service_healthy") {
			t.Errorf("%s does not preserve Compose readiness", path)
		}
		if !strings.Contains(contents, "cat /tmp/readiness-error >&2") {
			t.Errorf("%s does not expose the final S3 readiness error", path)
		}
	}
	if matched == 0 {
		t.Fatal("no RustFS Compose examples found")
	}

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`rustfs/rustfs:[\w.-]+@sha256:[0-9a-f]{64}`).FindAllString(string(readme), -1)
	if len(refs) == 0 {
		t.Fatal("README contains no RustFS image reference")
	}
	for _, ref := range refs {
		if ref != RustFS {
			t.Errorf("README RustFS image %q differs from %q", ref, RustFS)
		}
	}
}
