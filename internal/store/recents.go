package store

import (
	"encoding/json"
	"os"
	"time"
)

// recent is one persisted recents.json entry, byte-shape-compatible with Swift's
// Recent (Clipboard.swift:603-607): paths only, non-secret. Time is RFC3339/ISO8601
// to match Swift's JSONEncoder .iso8601 strategy so the two apps can read each other's
// recents.json on a shared ~/.clipbeam.
type recent struct {
	Path   string    `json:"path"`
	Sender string    `json:"sender"`
	Time   time.Time `json:"time"`
}

// recentsCap is the ring size: keep the last 20 (PLAN §7.2, Swift Clipboard.swift:616).
const recentsCap = 20

// appendRecents appends paths (sender-tagged, now-stamped) to the recents ring at
// recentsFile, keeps the last 20, writes atomically at 0600, and tolerates a corrupt
// file by resetting — byte-for-behavior with Swift appendRecents (Clipboard.swift:
// 611-623). Empty paths is a no-op (matches Swift's guard). Best-effort: a write
// failure is swallowed (Swift uses try?), recents is a convenience, not load-bearing.
func appendRecents(recentsFile string, paths []string, sender string) {
	if len(paths) == 0 {
		return
	}
	recents := loadRecents(recentsFile)
	now := time.Now()
	for _, p := range paths {
		recents = append(recents, recent{Path: p, Sender: sender, Time: now})
	}
	if len(recents) > recentsCap {
		recents = recents[len(recents)-recentsCap:]
	}
	data, err := json.Marshal(recents)
	if err != nil {
		return
	}
	_ = writeBytesAtomic(recentsFile, data, 0o600)
}

// loadRecents reads and decodes the recents ring, returning an empty slice on a
// missing or corrupt file (corruption-tolerant reset — the next write overwrites).
func loadRecents(recentsFile string) []recent {
	data, err := os.ReadFile(recentsFile)
	if err != nil {
		return nil
	}
	var recents []recent
	if err := json.Unmarshal(data, &recents); err != nil {
		return nil // tolerate corruption by resetting
	}
	return recents
}
