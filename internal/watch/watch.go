// Package watch watches a fixed set of files and invokes a callback, debounced,
// when any of them changes. It backs localfront's template / seed hot reload.
package watch

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceInterval coalesces the burst of events editors emit on a single save
// (write + rename + chmod) into one reload.
const debounceInterval = 200 * time.Millisecond

// Watch watches the given files and calls onChange once per debounced change
// burst until ctx is cancelled. It watches the parent directories (editors
// often replace files via rename, which drops watches on the inode) and filters
// events down to the requested files. It returns when ctx is done.
func Watch(ctx context.Context, files []string, logger *slog.Logger, onChange func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	watched := map[string]bool{}
	dirs := map[string]bool{}
	for _, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return err
		}
		watched[abs] = true
		dirs[filepath.Dir(abs)] = true
	}
	for dir := range dirs {
		if err := w.Add(dir); err != nil {
			return err
		}
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return nil
			}
			abs, err := filepath.Abs(event.Name)
			if err != nil || !watched[abs] {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			// Reset the debounce timer.
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(debounceInterval)
			timerC = timer.C
		case <-timerC:
			timerC = nil
			onChange()
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			logger.Warn("file watch error", "error", err)
		}
	}
}
