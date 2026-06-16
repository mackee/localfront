package watch

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// syncWatcherReady mutates the watched file until the callback fires, proving
// that fsnotify finished registering. Without it the test could write its
// real probe before Watch reaches its event loop and miss the event. The
// per-attempt wait is longer than the debounce window so a single write is
// not coalesced away by a subsequent retry.
func syncWatcherReady(t *testing.T, file string, fired <-chan struct{}) {
	t.Helper()
	const perAttempt = debounceInterval + 100*time.Millisecond
	const maxWait = 5 * time.Second
	attempts := int(maxWait / perAttempt)
	for i := range attempts {
		if err := os.WriteFile(file, []byte("sync-"+strconv.Itoa(i)), 0o600); err != nil {
			t.Fatalf("sync write: %v", err)
		}
		select {
		case <-fired:
			drainFires(fired)
			return
		case <-time.After(perAttempt):
			// no fire yet — try another write
		}
	}
	t.Fatal("watcher never confirmed registration within deadline")
}

// drainFires consumes any debounced fires the sync writes produced, so the
// test's actual probe starts from an empty channel.
func drainFires(fired <-chan struct{}) {
	for {
		select {
		case <-fired:
		case <-time.After(2 * debounceInterval):
			return
		}
	}
}

func TestWatch_FiresOnChange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(file, []byte("initial"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fired := make(chan struct{}, 8)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		_ = Watch(t.Context(), []string{file}, logger, func() {
			select {
			case fired <- struct{}{}:
			default:
			}
		})
	}()

	syncWatcherReady(t, file, fired)

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

	fired := make(chan struct{}, 8)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		_ = Watch(t.Context(), []string{watched}, logger, func() {
			select {
			case fired <- struct{}{}:
			default:
			}
		})
	}()

	syncWatcherReady(t, watched, fired)

	// Touching an unwatched file in the same directory must not fire.
	if err := os.WriteFile(other, []byte("y"), 0o600); err != nil {
		t.Fatalf("rewrite other: %v", err)
	}
	// Wait the debounce window plus slack — any fire that was going to happen
	// would have arrived by now.
	select {
	case <-fired:
		t.Error("onChange fired for an unwatched file, want no fire")
	case <-time.After(2 * debounceInterval):
	}
}
