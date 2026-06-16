package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mackee/localfront/internal/cfntmpl"
	"github.com/mackee/localfront/internal/config"
)

// TestExampleTemplatesLoad ensures every example template in examples/ parses
// and builds: it catches typos in managed policy IDs, intrinsics, and resource
// shapes before they reach a user.
func TestExampleTemplatesLoad(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "template.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no example templates found")
	}
	for _, path := range matches {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			cfg, err := config.Load([]cfntmpl.Source{{Name: path, Data: data}}, nil)
			if err != nil {
				t.Fatalf("load %s: %v", path, err)
			}
			if len(cfg.Distributions) == 0 {
				t.Errorf("%s produced no distributions", path)
			}
		})
	}
}
