package watch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatch_FiresOnChange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(file, []byte("initial"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var count atomic.Int32
	fired := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		_ = Watch(ctx, []string{file}, logger, func() {
			count.Add(1)
			select {
			case fired <- struct{}{}:
			default:
			}
		})
	}()

	// Give the watcher time to register before mutating the file.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(file, []byte("changed"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("onChange was not called within 5s of a file change")
	}
}

func TestWatch_IgnoresUnwatchedFiles(t *testing.T) {
	dir := t.TempDir()
	watched := filepath.Join(dir, "watched.yaml")
	other := filepath.Join(dir, "other.txt")
	for _, f := range []string{watched, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	var count atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		_ = Watch(ctx, []string{watched}, logger, func() { count.Add(1) })
	}()
	time.Sleep(100 * time.Millisecond)

	// Touching an unwatched file in the same directory must not fire.
	if err := os.WriteFile(other, []byte("y"), 0o600); err != nil {
		t.Fatalf("rewrite other: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if n := count.Load(); n != 0 {
		t.Errorf("onChange fired %d times for an unwatched file, want 0", n)
	}
}
