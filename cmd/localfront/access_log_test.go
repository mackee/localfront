package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAccessLog_Disabled(t *testing.T) {
	closer, w, err := openAccessLog("")
	if err != nil {
		t.Fatalf("openAccessLog: %v", err)
	}
	if closer != nil || w != nil {
		t.Fatalf("disabled access log should return (nil, nil, nil); got closer=%v writer=%v", closer, w)
	}
}

func TestOpenAccessLog_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	closer, w, err := openAccessLog(path)
	if err != nil {
		t.Fatalf("openAccessLog: %v", err)
	}
	if closer == nil || w == nil {
		t.Fatalf("file access log should return non-nil closer + writer")
	}
	t.Cleanup(func() { _ = closer.Close() })

	// NewWriter wrote the W3C header; the file should contain it before any
	// requests have been served.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read access log: %v", err)
	}
	if !strings.HasPrefix(string(data), "#Version: 1.0\n") {
		t.Fatalf("access log file missing header preamble; got %q", string(data))
	}
}

func TestOpenAccessLog_Stdout(t *testing.T) {
	for _, name := range []string{"-", "stdout"} {
		closer, w, err := openAccessLog(name)
		if err != nil {
			t.Fatalf("openAccessLog(%q): %v", name, err)
		}
		if closer != nil {
			t.Errorf("openAccessLog(%q): stdout sink must not return a closer", name)
		}
		if w == nil {
			t.Errorf("openAccessLog(%q): writer must be non-nil", name)
		}
	}
}
