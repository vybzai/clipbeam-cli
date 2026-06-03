// Package store defines the ReceiveStore boundary and its skeleton implementation:
// the clipboard channel (disk save / last_path / recents) and the agent channel
// (in-memory FIFO inbox + long-poll waiters). The long-poll primitives are the
// per-waiter buffered chan + sync.Mutex model (PLAN §3.10), the Go analog of
// Swift ReceiveStore's per-waiter boxed continuations.
package store

import (
	"context"
	"io"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// ReceiveStore is the receive-side state boundary, implemented by the concrete
// store (M1b) and consumed by the httpd handlers (M1c). It is the Go analog of the
// Swift ClipReceiver protocol (Server.swift:204) split into its two channels.
//
// The clipboard channel writes disk + last_path + recents (PLAN §7.2); the agent
// channel enqueues to an in-memory FIFO drained one-at-a-time by Recv (PLAN §7.5).
// Neither method sets a system clipboard or notifies in v1 (PLAN §7.4).
type ReceiveStore interface {
	// SaveClipboardItem streams one clipboard-channel image/file item's payload from
	// r to the save dir (sanitize → temp 0600 → fsync → atomic rename →
	// collision-suffix), via io.CopyBuffer (32 KB) so a 50 MB item is never held
	// whole in RAM (PLAN §3.7). It returns the absolute saved path and the number of
	// decoded bytes written (so Ingest can do the per-item write→add→check, §3.8).
	// last_path/recents updates are deferred to FinishClipboard.
	SaveClipboardItem(name, kind, uti string, r io.Reader) (path string, written int64, err error)

	// SaveClipboardText handles a clipboard-channel text item. It writes a .txt
	// sidecar (and returns its path) only when saveTextToDisk OR utf8 >
	// longTextThreshold; otherwise it returns ("", false, 0, nil) — the short-text
	// carve-out that is NOT counted toward maxBytes (PLAN §3.8, §7.2). written is the
	// sidecar byte count (0 when no sidecar is written).
	SaveClipboardText(text string) (path string, wroteSidecar bool, written int64, err error)

	// FinishClipboard commits last_path (no trailing newline) to the most-recent
	// saved path and appends savedPaths to recents (last 20, 0600), then resumes any
	// parked WaitForNext waiter (PLAN §7.2). Called once after all items are saved.
	FinishClipboard(sender string, savedPaths []string) error

	// SaveAgentItem streams one agent-channel image/file item from r to the agent
	// inbox (dir 0700, files 0600) and enqueues an AgentItem carrying its path.
	// Returns the absolute saved path and the decoded byte count (PLAN §7.5). The
	// payload file is also recorded in the durable on-disk journal ONLY when the store
	// was built with JournalAgentItems=true (the daemonless `clipbeam ingest` verb,
	// fix [F]); the serve daemon builds the store WITHOUT that flag, so it stays
	// in-memory-FIFO-only with no journal write (no pileup, no double-delivery, H1/H2).
	SaveAgentItem(sender, name, kind, uti string, r io.Reader) (path string, written int64, err error)

	// EnqueueAgentText enqueues an agent-channel text item in the in-memory FIFO. It is
	// ALSO written to the durable on-disk journal ONLY when the store was built with
	// JournalAgentItems=true (the daemonless `clipbeam ingest` verb, fix [F]); the serve
	// daemon builds the store WITHOUT that flag, so text stays in memory only — never
	// written to disk (PLAN §7.5; no plaintext retention, no double-delivery, H1/H2).
	EnqueueAgentText(sender, text string) error

	// LastPath returns the most-recently-saved clipboard path, or ("", false) if
	// none. Powers GET /last (PLAN §7.7).
	LastPath() (string, bool)

	// Recv dequeues exactly one agent FIFO item, or parks until ctx is done
	// (timeout → nil item). Powers GET /recv (PLAN §3.10).
	Recv(ctx context.Context) (*wire.AgentItem, error)

	// DrainAgentDisk atomically claims and returns the OLDEST durable agent-channel
	// journal record (FIFO), or a nil item when the journal is empty. It is a ONE-SHOT
	// disk drain (NOT a long-poll) used by the daemonless `clipbeam recv` fallback when
	// no daemon is reachable (fix [F]); it is exactly-once across concurrent drainers.
	DrainAgentDisk(ctx context.Context) (*wire.AgentItem, error)

	// WaitForNext parks until the next clipboard path is saved or ctx is done
	// (latest-wins, not a queue). Powers the fixed-120 s GET /wait (PLAN §3.10).
	WaitForNext(ctx context.Context) (string, error)
}

// New constructs the concrete ReceiveStore over the given save dir, agent inbox dir,
// last_path file, recents file, and the text-sidecar policy. The in-memory agent
// FIFO + clipboard waiter primitives (PLAN §3.10) and the bounded-memory disk-save
// pipeline (sanitize → temp 0600 → fsync → atomic rename → collision, §3.7) are fully
// wired. Any persisted last_path is loaded so GET /last survives a restart.
func New(cfg StoreConfig) (ReceiveStore, error) {
	return newConcrete(cfg), nil
}

// StoreConfig parameterizes the concrete store with the per-OS paths and the
// text-counting policy (PLAN §3.8, §4).
type StoreConfig struct {
	SaveDir           string
	AgentInboxDir     string
	LastPathFile      string
	RecentsFile       string
	SaveTextToDisk    bool
	LongTextThreshold int
	MaxBytes          int

	// JournalAgentItems opts the store into the durable on-disk agent journal (fix [F]).
	// It is set TRUE only by the one-shot, daemonless `clipbeam ingest` verb (a
	// short-lived process whose in-memory FIFO is discarded on exit, so a later
	// daemonless `clipbeam recv` — a DIFFERENT process — drains the journal instead).
	// It is FALSE for the serve daemon and any HTTP /clip,/agent-send handler path: the
	// daemon is the live consumer of its own in-memory FIFO via /recv, so it must NOT
	// also write the journal (that would grow unbounded with retained plaintext, H1, and
	// double-deliver to a later daemonless recv, H2). Default FALSE keeps the daemon
	// path byte-for-byte its pre-changeset in-memory-only behavior.
	JournalAgentItems bool
}
