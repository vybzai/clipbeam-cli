package store

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vybzai/clipbeam-cli/internal/sanitize"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// receiveStore is the concrete ReceiveStore (PLAN §3.10, §7). It owns the agent FIFO
// + clipboard path-waiter primitives, the per-OS paths/policy, and the disk-save
// pipeline (sanitize → temp 0600 → fsync → atomic rename → collision-suffix). The
// sanitizer is injectable so tests can drive the containment / write-failure seam
// (PLAN §12.4); production uses sanitize.Default().
type receiveStore struct {
	cfg       StoreConfig
	sanitizer sanitize.Sanitizer

	agent    agentFIFO
	clipWait pathWaiters

	mu       sync.Mutex // guards lastPath and the on-disk save-dir collision races
	lastPath string
}

// newConcrete builds the concrete store, loading any persisted last_path so GET /last
// survives a restart (Swift lastPath() reads the file lazily; we cache it at start and
// keep it fresh on every FinishClipboard).
func newConcrete(cfg StoreConfig) *receiveStore {
	s := &receiveStore{cfg: cfg, sanitizer: sanitize.Default()}
	s.lastPath = loadLastPath(cfg.LastPathFile)
	return s
}

// SaveClipboardItem streams one clipboard-channel image/file payload into the save dir
// (sanitize → temp 0600 → fsync → atomic rename → collision-suffix) via io.CopyBuffer
// (32 KB) so a 50 MB item is never held whole in RAM (PLAN §3.7). It returns the
// absolute saved path and the bytes written. last_path/recents are deferred to
// FinishClipboard.
func (s *receiveStore) SaveClipboardItem(name, kind, uti string, r io.Reader) (string, int64, error) {
	return s.saveStream(s.cfg.SaveDir, name, kind, uti, r, false)
}

// SaveClipboardText handles a clipboard-channel text item. It writes a .txt sidecar
// (and returns its path) ONLY when saveTextToDisk OR utf8 > longTextThreshold;
// otherwise it returns ("", false, 0, nil) — the short-text carve-out that Ingest does
// NOT count toward maxBytes (PLAN §3.8, §7.2). The sidecar name is the sanitized
// clipbeam-<UTC>.txt; written is its UTF-8 byte count.
func (s *receiveStore) SaveClipboardText(text string) (string, bool, int64, error) {
	if !s.cfg.SaveTextToDisk && len(text) <= s.cfg.LongTextThreshold {
		return "", false, 0, nil
	}
	// The sidecar rides the SAME save pipeline (sanitize + atomic + collision) as a
	// file, matching Swift saveData(textFileName(), kind:.file) (Clipboard.swift:255).
	path, n, err := s.saveStream(s.cfg.SaveDir, sanitize.TextFileName(), sanitize.KindFile, "public.plain-text", strings.NewReader(text), false)
	if err != nil {
		return "", false, 0, err
	}
	return path, true, n, nil
}

// FinishClipboard commits last_path (no trailing newline, atomic) to the most-recent
// saved path, appends savedPaths to recents (last 20, 0600, corruption-tolerant), and
// resumes any parked /wait waiter (PLAN §7.2). Called once after all items are saved.
func (s *receiveStore) FinishClipboard(sender string, savedPaths []string) error {
	if len(savedPaths) > 0 {
		last := savedPaths[len(savedPaths)-1]
		s.mu.Lock()
		s.lastPath = last
		s.mu.Unlock()
		// last_path = raw path bytes, NO trailing newline (Swift Data(path.utf8),
		// Clipboard.swift:599 — the shim's $(clipbeam last) substitution depends on it).
		_ = writeBytesAtomic(s.cfg.LastPathFile, []byte(last), 0o600)
		appendRecents(s.cfg.RecentsFile, savedPaths, sender)
		s.clipWait.resume(last)
	}
	return nil
}

// SaveAgentItem streams one agent-channel image/file payload into the agent inbox
// (dir 0700, files 0600), enqueues an AgentItem carrying its path, and returns the
// absolute saved path + bytes written (PLAN §7.5). Never touches last_path/recents.
func (s *receiveStore) SaveAgentItem(sender, name, kind, uti string, r io.Reader) (string, int64, error) {
	path, n, err := s.saveStream(s.cfg.AgentInboxDir, name, kind, uti, r, true)
	if err != nil {
		return "", 0, err
	}
	atype := "file"
	if kind == sanitize.KindImage {
		atype = "image"
	}
	p := path
	s.agent.enqueue(wire.AgentItem{Type: atype, Sender: sender, Path: &p})
	return path, n, nil
}

// EnqueueAgentText enqueues an in-memory-only agent text item — never written to disk
// (PLAN §7.5).
func (s *receiveStore) EnqueueAgentText(sender, text string) error {
	t := text
	s.agent.enqueue(wire.AgentItem{Type: "text", Sender: sender, Text: &t})
	return nil
}

// LastPath returns the most-recently-saved clipboard path, or ("", false) if none.
func (s *receiveStore) LastPath() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPath == "" {
		return "", false
	}
	return s.lastPath, true
}

// Recv dequeues one agent FIFO item, parking until ctx is done (PLAN §3.10).
func (s *receiveStore) Recv(ctx context.Context) (*wire.AgentItem, error) {
	return s.agent.recv(ctx)
}

// WaitForNext parks until the next clipboard path is saved or ctx is done (PLAN §3.10).
func (s *receiveStore) WaitForNext(ctx context.Context) (string, error) {
	return s.clipWait.wait(ctx)
}

// saveStream is the shared save pipeline for both the clipboard save dir and the agent
// inbox. It (1) ensures the dir exists with the right mode (inbox: 0700), (2) runs the
// sanitizer to get a containment-checked leaf, (3) resolves collisions by stat-probing
// " (2)", " (3)" candidates under the dir mutex, and (4) streams the payload via
// writeAtomicFsync. It returns the absolute final path and the bytes written.
func (s *receiveStore) saveStream(dir, name, kind, uti string, r io.Reader, isInbox bool) (string, int64, error) {
	if isInbox {
		// Agent inbox: 0700 dir, created on first use and re-tightened (Swift
		// ensureAgentInbox, Clipboard.swift:412-419).
		_ = os.MkdirAll(dir, 0o700)
		_ = os.Chmod(dir, 0o700)
	} else {
		_ = os.MkdirAll(dir, 0o700)
	}

	leaf, err := s.sanitizer.SafeLeaf(name, kind, uti, dir)
	if err != nil {
		return "", 0, err // ErrPathEscape stays generic upstream
	}

	// Resolve the collision and write under the dir mutex so two concurrent saves of
	// the same leaf cannot both pick the same name between the stat and the rename.
	s.mu.Lock()
	final := s.uniqueLeaf(dir, leaf)
	dest := filepath.Join(dir, final)
	// Reserve the name with an exclusive create so a racing save sees it taken.
	if reserveErr := reserve(dest); reserveErr != nil {
		// A late collision (someone reserved between uniqueLeaf and reserve) — retry
		// once by re-probing; this is rare and bounded.
		final = s.uniqueLeaf(dir, leaf)
		dest = filepath.Join(dir, final)
		_ = reserve(dest)
	}
	s.mu.Unlock()

	written, err := writeAtomicFsync(dest, r)
	if err != nil {
		_ = os.Remove(dest)
		return "", written, err
	}
	return dest, written, nil
}

// uniqueLeaf returns leaf if dir/leaf does not exist, else the first free
// " (n)"-suffixed variant (n≥2), byte-for-behavior with Swift uniqueURL
// (Clipboard.swift:466-478). Caller holds s.mu.
func (s *receiveStore) uniqueLeaf(dir, leaf string) string {
	if !exists(filepath.Join(dir, leaf)) {
		return leaf
	}
	for n := 2; ; n++ {
		cand := sanitize.CollisionName(leaf, n)
		if !exists(filepath.Join(dir, cand)) {
			return cand
		}
	}
}

// reserve atomically claims dest with an exclusive 0600 create so a concurrent saver
// observes it as taken; the subsequent writeAtomicFsync renames over this placeholder.
func reserve(dest string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// exists reports whether path exists (any stat error other than not-exist is treated
// as exists, the conservative choice that mirrors Swift fileExists).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// loadLastPath reads the persisted last_path (raw bytes, no trailing newline), or ""
// if absent/empty/unreadable (Swift lastPath() with a nil-on-error guard).
func loadLastPath(file string) string {
	if file == "" {
		return ""
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	// Swift writes no trailing newline, but tolerate one if a human edited the file.
	return strings.TrimRight(string(data), "\n")
}
