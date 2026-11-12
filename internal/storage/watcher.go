package storage

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Watcher monitors one or more directories for supported book file changes
// at a regular interval (.epub and .m4b, by IsSupportedFile).
type Watcher struct {
	dirs     []string
	interval time.Duration
	onChange func(paths []string)
	onError  func(errs []string)
	stop     chan struct{}
	known    map[string]time.Time
	mu       sync.Mutex
}

// NewWatcher creates a watcher for a single directory.
func NewWatcher(dir string, interval time.Duration, onChange func([]string), onError func([]string)) *Watcher {
	return NewMultiWatcher([]string{dir}, interval, onChange, onError)
}

// NewMultiWatcher creates a watcher that scans several directories. Empty
// strings are skipped, so callers can pass conditionally-set paths directly.
// onError is called after every scan with the current set of walk errors
// (e.g. an unmounted drive or a permission-denied path) — nil or empty when
// the scan was clean. It fires whether or not the file set changed, so a
// persistent problem (like a library path that no longer exists) keeps
// being reported rather than only surfacing once.
func NewMultiWatcher(dirs []string, interval time.Duration, onChange func([]string), onError func([]string)) *Watcher {
	clean := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d != "" {
			clean = append(clean, d)
		}
	}
	return &Watcher{
		dirs:     clean,
		interval: interval,
		onChange: onChange,
		onError:  onError,
		stop:     make(chan struct{}),
		known:    make(map[string]time.Time),
	}
}

// Start begins the polling loop in a goroutine. Call Stop to end it.
func (w *Watcher) Start() {
	go w.loop()
}

// Stop ends the polling loop.
func (w *Watcher) Stop() {
	close(w.stop)
}

func (w *Watcher) loop() {
	w.scan()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.scan()
		}
	}
}

func (w *Watcher) scan() {
	current := make(map[string]time.Time)
	var errs []string
	for _, dir := range w.dirs {
		filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				log.Printf("watcher: scan error at %s: %v", p, err)
				errs = append(errs, fmt.Sprintf("%s: %v", p, err))
				return nil // skip this entry, keep walking the rest
			}
			if info == nil {
				return nil
			}
			if info.IsDir() && IsSkippedDir(info.Name()) {
				return filepath.SkipDir
			}
			if !info.IsDir() && IsSupportedFile(p) {
				current[p] = info.ModTime()
			}
			return nil
		})
	}

	if w.onError != nil {
		w.onError(errs)
	}

	w.mu.Lock()
	changed := len(current) != len(w.known)
	if !changed {
		for k, v := range current {
			if prev, ok := w.known[k]; !ok || !prev.Equal(v) {
				changed = true
				break
			}
		}
	}

	var paths []string
	if changed {
		w.known = current
		paths = make([]string, 0, len(current))
		for p := range current {
			paths = append(paths, p)
		}
		sort.Strings(paths)
	}
	w.mu.Unlock()

	if changed {
		log.Printf("watcher: detected %d file(s) across %d dir(s)", len(paths), len(w.dirs))
		w.onChange(paths)
	}
}
