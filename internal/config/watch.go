package config

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const reloadDebounce = 150 * time.Millisecond

// Watch watches the directory containing path for changes and calls onChange
// once changes settle, debounced by reloadDebounce. Watch blocks until ctx is
// canceled or the underlying watcher fails.
func Watch(ctx context.Context, path string, onChange func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config watcher: %w", err)
	}
	defer watcher.Close()

	watchDir := filepath.Dir(path)
	if err := watcher.Add(watchDir); err != nil {
		return fmt.Errorf("watch config directory %q: %w", watchDir, err)
	}

	targetPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve config path %q: %w", path, err)
	}

	timer := time.NewTimer(reloadDebounce)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return fmt.Errorf("config watcher events channel closed unexpectedly")
			}
			eventPath, err := filepath.Abs(event.Name)
			if err != nil {
				continue
			}
			if eventPath != targetPath {
				continue
			}
			timer.Reset(reloadDebounce)
			timerC = timer.C
		case <-timerC:
			onChange()
			timerC = nil

		case err, ok := <-watcher.Errors:
			if !ok {
				return fmt.Errorf("config watcher errors channel closed unexpectedly")
			}
			return fmt.Errorf("config watcher error: %w", err)
		}
	}
}
