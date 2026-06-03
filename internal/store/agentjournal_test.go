package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// newJournalStore builds a concrete store over a temp agent inbox for journal tests. It
// opts into the on-disk journal (JournalAgentItems=true), as the daemonless `clipbeam
// ingest` verb does — the ONLY path that journals (fix [F], H1/H2 gating).
func newJournalStore(t *testing.T, inbox string) *receiveStore {
	t.Helper()
	dir := t.TempDir()
	return newConcrete(StoreConfig{
		SaveDir:           filepath.Join(dir, "save"),
		AgentInboxDir:     inbox,
		LastPathFile:      filepath.Join(dir, "last_path"),
		RecentsFile:       filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192,
		MaxBytes:          wire.DefaultMaxBytes,
		JournalAgentItems: true,
	})
}

// TestAgentJournalPersistsTextAndFile asserts EnqueueAgentText and SaveAgentItem each
// leave a durable journal record with the correct type/hasText/text/path, and that a
// text:"" item records hasText=true,text="" (fix [F] T1).
func TestAgentJournalPersistsTextAndFile(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	s := newJournalStore(t, inbox)

	if err := s.EnqueueAgentText("", "hello agent"); err != nil {
		t.Fatalf("EnqueueAgentText: %v", err)
	}
	// A present-but-empty text item.
	if err := s.EnqueueAgentText("", ""); err != nil {
		t.Fatalf("EnqueueAgentText empty: %v", err)
	}
	if _, _, err := s.SaveAgentItem("", "shot.png", "image", "public.png", strings.NewReader("PNGDATA")); err != nil {
		t.Fatalf("SaveAgentItem: %v", err)
	}

	names, err := listJournalRecords(journalDir(inbox))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 3 {
		t.Fatalf("got %d journal records, want 3", len(names))
	}

	// Drain in FIFO order and assert each record's shape.
	ctx := context.Background()
	it1, _ := s.DrainAgentDisk(ctx)
	if it1 == nil || it1.Type != "text" || it1.Text == nil || *it1.Text != "hello agent" {
		t.Fatalf("record 1 = %+v, want text 'hello agent'", it1)
	}
	it2, _ := s.DrainAgentDisk(ctx)
	if it2 == nil || it2.Type != "text" || it2.Text == nil || *it2.Text != "" {
		t.Fatalf("record 2 = %+v, want present-but-empty text", it2)
	}
	it3, _ := s.DrainAgentDisk(ctx)
	if it3 == nil || it3.Type != "image" || it3.Path == nil || it3.Text != nil {
		t.Fatalf("record 3 = %+v, want image with a path and NO text", it3)
	}
	// Path points into the agent inbox.
	if !strings.HasPrefix(*it3.Path, inbox) {
		t.Errorf("image path %q is not under the agent inbox %q", *it3.Path, inbox)
	}
}

// TestDrainAgentDiskFIFOAcrossProcesses writes via store A, then a FRESH store B over the
// SAME inbox (simulating a separate-process recv) drains them in FIFO order and then nil
// (fix [F] T2).
func TestDrainAgentDiskFIFOAcrossProcesses(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	a := newJournalStore(t, inbox)
	for _, msg := range []string{"one", "two", "three"} {
		if err := a.EnqueueAgentText("", msg); err != nil {
			t.Fatal(err)
		}
	}

	b := newJournalStore(t, inbox) // separate "process"
	ctx := context.Background()
	for _, want := range []string{"one", "two", "three"} {
		it, err := b.DrainAgentDisk(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if it == nil || it.Text == nil || *it.Text != want {
			t.Fatalf("drained %+v, want %q (FIFO)", it, want)
		}
	}
	it, err := b.DrainAgentDisk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if it != nil {
		t.Fatalf("drained %+v after empty, want nil", it)
	}
}

// TestDrainAgentDiskExactlyOnceConcurrent enqueues N items, launches 2 concurrent
// drainers each draining to nil, and asserts the union is exactly the N items with no
// duplicates (rename-claim exactly-once, fix [F] T3).
func TestDrainAgentDiskExactlyOnceConcurrent(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	s := newJournalStore(t, inbox)
	const n = 50
	for i := 0; i < n; i++ {
		if err := s.EnqueueAgentText("", "msg"); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := 0
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := newJournalStore(t, inbox)
			ctx := context.Background()
			for {
				it, err := d.DrainAgentDisk(ctx)
				if err != nil || it == nil {
					return
				}
				mu.Lock()
				seen++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if seen != n {
		t.Fatalf("drained %d items total across 2 drainers, want exactly %d (no dupes, no drops)", seen, n)
	}
}

// TestDrainPreservesEmptyTextDistinctFromAbsent asserts a text:"" item drains to
// Text==&"" (not nil) while an image item drains to Text==nil (fix [F] T4).
func TestDrainPreservesEmptyTextDistinctFromAbsent(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	s := newJournalStore(t, inbox)
	if err := s.EnqueueAgentText("", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SaveAgentItem("", "f.bin", "file", "public.data", strings.NewReader("X")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	textItem, _ := s.DrainAgentDisk(ctx)
	if textItem == nil || textItem.Text == nil || *textItem.Text != "" {
		t.Fatalf("empty-text item = %+v, want Text==&\"\" (not nil)", textItem)
	}
	fileItem, _ := s.DrainAgentDisk(ctx)
	if fileItem == nil || fileItem.Text != nil {
		t.Fatalf("file item = %+v, want Text==nil (absent)", fileItem)
	}
	if fileItem.Path == nil {
		t.Fatalf("file item must carry a path")
	}
}

// newDaemonStore builds a concrete store over a temp agent inbox in DAEMON mode
// (JournalAgentItems=false) — the serve daemon's construction, the live /recv consumer of
// its own in-memory FIFO (fix [F] H1/H2 gating).
func newDaemonStore(t *testing.T, inbox string) *receiveStore {
	t.Helper()
	dir := t.TempDir()
	return newConcrete(StoreConfig{
		SaveDir:           filepath.Join(dir, "save"),
		AgentInboxDir:     inbox,
		LastPathFile:      filepath.Join(dir, "last_path"),
		RecentsFile:       filepath.Join(dir, "recents.json"),
		LongTextThreshold: 8192,
		MaxBytes:          wire.DefaultMaxBytes,
		JournalAgentItems: false,
	})
}

// TestDaemonModeWritesNoJournal is the H1/H2 gate, test (a): a store built in DAEMON mode
// (JournalAgentItems=false) must write NO journal record for either an agent text or an
// agent file/image item — the in-memory FIFO is the ONLY queue, so no plaintext piles up
// on disk (H1) and there is nothing for a later daemonless recv to re-deliver (H2). The
// item is STILL enqueued and Recv-able from memory (the daemon's live delivery path).
func TestDaemonModeWritesNoJournal(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")
	s := newDaemonStore(t, inbox)

	if err := s.EnqueueAgentText("", "secret plaintext"); err != nil {
		t.Fatalf("EnqueueAgentText: %v", err)
	}
	if _, _, err := s.SaveAgentItem("", "shot.png", "image", "public.png", strings.NewReader("PNGDATA")); err != nil {
		t.Fatalf("SaveAgentItem: %v", err)
	}

	// The journal dir must be empty or absent (no journal record at all).
	names, err := listJournalRecords(journalDir(inbox))
	if err != nil {
		t.Fatalf("listJournalRecords: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("daemon-mode store wrote %d journal record(s), want 0 (no plaintext pileup, H1)", len(names))
	}
	if _, statErr := os.Stat(journalDir(inbox)); statErr == nil {
		t.Errorf("daemon-mode store created the journal dir %q, want it never touched", journalDir(inbox))
	}

	// The items are STILL on the in-memory FIFO (the daemon's live /recv path is intact).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	it1, _ := s.Recv(ctx)
	if it1 == nil || it1.Type != "text" || it1.Text == nil || *it1.Text != "secret plaintext" {
		t.Fatalf("daemon recv 1 = %+v, want the text item from the in-memory FIFO", it1)
	}
	it2, _ := s.Recv(ctx)
	if it2 == nil || it2.Type != "image" || it2.Path == nil {
		t.Fatalf("daemon recv 2 = %+v, want the image item from the in-memory FIFO", it2)
	}
}

// TestDaemonDeliveredItemNotRedeliveredDaemonless is the H2 gate, test (c): an item the
// DAEMON delivered from its in-memory FIFO must NOT be re-delivered by a subsequent
// daemonless recv, because the daemon (JournalAgentItems=false) never journaled it. A
// FRESH store over the SAME inbox (the separate daemonless `clipbeam recv` process)
// finds an empty journal and drains nothing.
func TestDaemonDeliveredItemNotRedeliveredDaemonless(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")

	// Daemon receives + delivers an item from its in-memory FIFO.
	d := newDaemonStore(t, inbox)
	if err := d.EnqueueAgentText("", "already delivered by the daemon"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, _ := d.Recv(ctx)
	if got == nil || got.Text == nil || *got.Text != "already delivered by the daemon" {
		t.Fatalf("daemon Recv = %+v, want the in-memory item", got)
	}

	// A separate daemonless recv process drains the on-disk journal — which is empty,
	// because the daemon never wrote it. No double-delivery.
	recv := newJournalStore(t, inbox) // a real daemonless drainer (journal-enabled writer/reader)
	it, err := recv.DrainAgentDisk(context.Background())
	if err != nil {
		t.Fatalf("DrainAgentDisk: %v", err)
	}
	if it != nil {
		t.Fatalf("daemonless recv re-delivered %+v, want nil (the daemon never journaled it, H2)", it)
	}
}

// TestDaemonlessIngestThenRecvDrainsExactlyOnce is the H1/H2 gate, test (b): the
// daemonless ingest path (JournalAgentItems=true) journals BOTH a text and a file item; a
// separate-process daemonless recv drains each exactly once (FIFO), the journal is then
// empty, and a SECOND recv returns no item (no double-delivery). Verifies the drain works
// for both text and file/image after the gating.
func TestDaemonlessIngestThenRecvDrainsExactlyOnce(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "inbox")

	// Daemonless `clipbeam ingest` (a short-lived process) journals a text + a file item.
	ingestProc := newJournalStore(t, inbox)
	if err := ingestProc.EnqueueAgentText("", "msg one"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ingestProc.SaveAgentItem("", "doc.bin", "file", "public.data", strings.NewReader("FILEBYTES")); err != nil {
		t.Fatal(err)
	}
	// Both are durable on disk after the ingest process exits.
	if names, _ := listJournalRecords(journalDir(inbox)); len(names) != 2 {
		t.Fatalf("ingest journaled %d records, want 2 (text + file)", len(names))
	}

	// A separate-process daemonless recv drains each exactly once, in FIFO order.
	recvProc := newJournalStore(t, inbox)
	ctx := context.Background()
	first, err := recvProc.DrainAgentDisk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first.Type != "text" || first.Text == nil || *first.Text != "msg one" {
		t.Fatalf("first drain = %+v, want the text item (FIFO)", first)
	}
	second, err := recvProc.DrainAgentDisk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Type != "file" || second.Path == nil {
		t.Fatalf("second drain = %+v, want the file item with a path", second)
	}
	if !strings.HasPrefix(*second.Path, inbox) {
		t.Errorf("file path %q is not under the agent inbox %q", *second.Path, inbox)
	}

	// The journal is now empty, and a THIRD recv returns no item (no double-delivery).
	if names, _ := listJournalRecords(journalDir(inbox)); len(names) != 0 {
		t.Fatalf("journal has %d records after draining both, want 0", len(names))
	}
	third, err := recvProc.DrainAgentDisk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Fatalf("third drain = %+v, want nil (exactly-once, no double-delivery)", third)
	}
}

// TestDrainAgentDiskEmptyDirNoError asserts draining a never-used inbox (no journal dir)
// returns a nil item and no error.
func TestDrainAgentDiskEmptyDirNoError(t *testing.T) {
	inbox := filepath.Join(t.TempDir(), "never-used")
	s := newJournalStore(t, inbox)
	it, err := s.DrainAgentDisk(context.Background())
	if err != nil || it != nil {
		t.Fatalf("empty inbox drain = (%+v,%v), want (nil,nil)", it, err)
	}
	// The journal dir mode is 0700 when created by a write.
	if err := s.EnqueueAgentText("", "x"); err != nil {
		t.Fatal(err)
	}
	fi, serr := os.Stat(journalDir(inbox))
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("journal dir mode = %v, want 0700", fi.Mode().Perm())
	}
}
