package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchCallsOnChangeOnFileWrite verifies that saving the watched config
// file triggers onChange, and that Watch returns cleanly once ctx is canceled.
func TestWatchCallsOnChangeOnFileWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("rules: []\n"), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	notified := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchErr := make(chan error, 1)
	go func() {
		watchErr <- Watch(ctx, path, func() { notified <- struct{}{} })
	}()

	// Give fsnotify a moment to register the directory before we write,
	// so the write isn't racing the watcher's own setup.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(path, []byte("rules: []\n# updated\n"), 0o644); err != nil {
		t.Fatalf("update config: %v", err)
	}

	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("onChange was not called after the config file was written")
	}

	cancel()
	select {
	case err := <-watchErr:
		if err != nil {
			t.Fatalf("Watch returned error after context cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch did not return promptly after context cancel")
	}
}

// TestWatchDebouncesRapidWrites verifies that a burst of writes within the
// debounce window collapses into exactly one onChange call, matching the
// debounced-reload design the hot-reload feature relies on.
func TestWatchDebouncesRapidWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("rules: []\n"), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Watch(ctx, path, func() { atomic.AddInt32(&calls, 1) })
	}()

	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 5; i++ {
		content := fmt.Sprintf("rules: []\n# revision %d\n", i)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write revision %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond) // well under reloadDebounce
	}

	// Wait past the debounce window so any pending onChange has fired.
	time.Sleep(reloadDebounce + 300*time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("onChange called %d times, want exactly 1 for a burst of writes inside the debounce window", got)
	}
}

// TestWatchIgnoresUnrelatedFile verifies that changes to other files in the
// same directory do not trigger onChange, since Watch must watch the
// directory (to survive atomic renames) without reacting to every file in it.
func TestWatchIgnoresUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("rules: []\n"), 0o644); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	notified := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = Watch(ctx, path, func() { notified <- struct{}{} })
	}()

	time.Sleep(50 * time.Millisecond)

	otherPath := filepath.Join(dir, "unrelated.txt")
	if err := os.WriteFile(otherPath, []byte("irrelevant"), 0o644); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}

	select {
	case <-notified:
		t.Fatal("onChange fired for a change to an unrelated file in the watched directory")
	case <-time.After(reloadDebounce + 300*time.Millisecond):
		// Expected: no notification within the debounce window.
	}
}
