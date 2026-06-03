package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// The agent channel is disk-backed so it works across a process boundary daemonlessly
// (fix [F]). Over SSH-exec, `clipbeam ingest` is a SHORT-LIVED process: it enqueues to
// an in-memory FIFO that is discarded on exit, so a later `clipbeam recv` (a DIFFERENT
// process) never sees it. The journal makes every agent item durable + ordered + typed
// so recv can drain it without a running daemon.
//
// STORAGE: one JSON sidecar per item under <AgentInboxDir>/journal/ named
// `<unixnanos>-<seq>.json`. The name is monotonic (wall-clock nanos) + a per-process
// atomic seq tiebreak, so a lexicographic directory sort IS FIFO order. Per-item files
// (vs one append-only journal) give a trivial atomic exactly-once claim via os.Rename and
// avoid rewrite/compaction races. Records are 0600 under a 0700 journal dir — the same
// secrecy model as the agent-inbox payloads.

// journalSeq is a per-process monotonic counter that breaks same-nanosecond ties so two
// rapid enqueues sort deterministically in FIFO order.
var journalSeq atomic.Uint64

// takenPrefix marks a journal record that a drainer has atomically claimed (renamed) so a
// concurrent drainer skips it. It is NOT a *.json name, so the listing glob never re-sees
// it.
const takenPrefix = ".taken-"

// agentJournalRecord is the durable form of a wire.AgentItem (fix [F]). hasText is stored
// EXPLICITLY so a present-but-empty text item (text:"") survives distinct from an absent
// text — preserving the wire.AgentItem.Text nil-vs-&"" invariant through the round-trip.
type agentJournalRecord struct {
	Type    string `json:"type"`           // "image" | "file" | "text"
	Sender  string `json:"sender"`         // "" for now (CB01 carries no sender)
	Path    string `json:"path,omitempty"` // abs path in agent-inbox (image/file)
	HasText bool   `json:"hasText"`        // true ⇒ Text is present (possibly "")
	Text    string `json:"text,omitempty"` // inline text (text items only)
}

// journalDir returns the journal subdir under the agent inbox.
func journalDir(agentInboxDir string) string {
	return filepath.Join(agentInboxDir, "journal")
}

// writeAgentJournal appends one durable journal record (fix [F] write side). It is
// additive to the in-memory enqueue, called from SaveAgentItem and EnqueueAgentText so
// text AND file/image items become durable for the daemonless disk-drain. The dir is
// created 0700 and the record file is written 0600 atomically. Errors are returned so the
// caller can decide; the ingest path treats a journal-write failure as fatal so a "saved"
// item is never silently undeliverable.
func writeAgentJournal(agentInboxDir, atype, sender, path string, hasText bool, text string) error {
	dir := journalDir(agentInboxDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	rec := agentJournalRecord{Type: atype, Sender: sender, Path: path, HasText: hasText, Text: text}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%020d.json", time.Now().UnixNano(), journalSeq.Add(1))
	return writeBytesAtomic(filepath.Join(dir, name), data, 0o600)
}

// DrainAgentDisk lists the journal records sorted by name (FIFO), atomically CLAIMS the
// OLDEST (os.Rename to a .taken-<name> sidecar), reconstructs a wire.AgentItem, and
// returns it; a nil item when the journal is empty (fix [F] drain side). It is a ONE-SHOT
// (not a long-poll) — there is no cross-process waiter daemonless. Concurrent drainers
// are exactly-once: only one Rename of a given name can succeed; the loser gets ENOENT
// and advances to the next file. After claiming an image/file item the payload file in
// the agent inbox is the durable deliverable and is left in place (recv returns its
// path).
func (s *receiveStore) DrainAgentDisk(ctx context.Context) (*wire.AgentItem, error) {
	dir := journalDir(s.cfg.AgentInboxDir)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		names, err := listJournalRecords(dir)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return nil, nil // empty journal — one-shot, no item
		}
		for _, name := range names {
			src := filepath.Join(dir, name)
			claimed := filepath.Join(dir, takenPrefix+name)
			if err := os.Rename(src, claimed); err != nil {
				// Lost the race (another drainer claimed it) or it vanished — try the next.
				continue
			}
			data, rerr := os.ReadFile(claimed)
			if rerr != nil {
				// Claimed but unreadable; drop the corrupt record and keep going.
				_ = os.Remove(claimed)
				continue
			}
			var rec agentJournalRecord
			if jerr := json.Unmarshal(data, &rec); jerr != nil {
				_ = os.Remove(claimed)
				continue
			}
			// The claim sidecar has served its purpose; remove it (the inbox payload, if
			// any, stays as the deliverable).
			_ = os.Remove(claimed)
			return recordToAgentItem(rec), nil
		}
		// Every candidate was lost to a concurrent claim; loop to re-list.
	}
}

// recordToAgentItem reconstructs a wire.AgentItem from a journal record, preserving the
// Text nil-vs-&"" distinction via the explicit HasText flag and Path nil-vs-set.
func recordToAgentItem(rec agentJournalRecord) *wire.AgentItem {
	it := &wire.AgentItem{Type: rec.Type, Sender: rec.Sender}
	if rec.Path != "" {
		p := rec.Path
		it.Path = &p
	}
	if rec.HasText {
		t := rec.Text
		it.Text = &t
	}
	return it
}

// listJournalRecords returns the *.json record names under dir sorted ascending (FIFO by
// the monotonic <nanos>-<seq> name). A missing dir yields an empty list (no error) — the
// journal simply has nothing yet.
func listJournalRecords(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// Only unclaimed *.json records; the .taken- sidecars are in-flight claims.
		if strings.HasPrefix(n, takenPrefix) || !strings.HasSuffix(n, ".json") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
