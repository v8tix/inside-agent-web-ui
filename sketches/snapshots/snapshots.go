// Package snapshots lets a fast producer feed a slow consumer: only the newest whole text matters.
package snapshots

import "sync"

// Latest holds the newest snapshot, which carries all the text so far.
type Latest struct {
	mu    sync.Mutex
	text  string
	fresh bool
}

// Publish stores a snapshot. A snapshot that nobody took yet is dropped, and nothing is lost.
func (l *Latest) Publish(text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text, l.fresh = text, true
}

// Take returns the newest snapshot, or false when nothing new came since the last Take.
func (l *Latest) Take() (text string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	text, ok, l.fresh = l.text, l.fresh, false
	return text, ok
}
