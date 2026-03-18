package storage

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Watcher monitors a directory for .epub file changes at a regular interval.
type Watcher struct {
	dir      string
	interval time.Duration
	onChange func(epubs []string)
	stop     chan struct{}
	known    map[string]time.Time
	mu       sync.Mutex
}

// NewWatcher creates a directory watcher that calls onChange when the set of
// .epub files changes. It polls at the given interval.
func NewWatcher(dir string, interval time.Duration, onChange func([]string)) *Watcher {
	return &Watcher{
		dir:      dir,
		interval: interval,
		onChange: onChange,
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
	// Initial scan
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
	err := filepath.Walk(w.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip inaccessible paths
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(p), ".epub") {
			current[p] = info.ModTime()
		}
		return nil
	})
	if err != nil {
		log.Printf("watcher: scan error: %v", err)
		return
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

	var epubs []string
	if changed {
		w.known = current
		epubs = make([]string, 0, len(current))
		for p := range current {
			epubs = append(epubs, p)
		}
	}
	w.mu.Unlock()

	if changed {
		log.Printf("watcher: detected %d epub(s) in %s", len(epubs), w.dir)
		w.onChange(epubs)
	}
}
