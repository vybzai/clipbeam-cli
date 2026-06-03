package ingest

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/classify"
)

// TestDefaultSanitizerReExport exercises the frozen ingest.DefaultSanitizer +
// ingest.TextFileName re-exports (PLAN §3.9 — kept under the ingest.* names so callers
// never reach into internal/sanitize directly). It asserts the sanitizer produces the
// verbatim leaf for a clean name and the text-sidecar name has the clipbeam- prefix +
// .txt suffix.
func TestDefaultSanitizerReExport(t *testing.T) {
	s := DefaultSanitizer()
	if s == nil {
		t.Fatal("DefaultSanitizer returned nil")
	}
	dir := t.TempDir()
	leaf, err := s.SafeLeaf("clean.png", KindImage, "public.png", dir)
	if err != nil {
		t.Fatalf("SafeLeaf clean name: %v", err)
	}
	if leaf != "clean.png" {
		t.Fatalf("SafeLeaf = %q, want clean.png (clean name passes verbatim)", leaf)
	}
	name := TextFileName()
	if !strings.HasPrefix(name, "clipbeam-") || !strings.HasSuffix(name, ".txt") {
		t.Fatalf("TextFileName = %q, want clipbeam-<UTC>.txt", name)
	}
}

// TestAgentFileSaveThenTextMidBatchCap drives ingestAgent's image/file branch AND the
// per-item write→add→check on the agent channel: a file item (counted) followed by a
// text item that pushes the running sum over maxBytes trips ErrTooLarge, with the file
// already on disk in the inbox (the durable deliverable, §3.8).
func TestAgentFileSaveThenTextMidBatchCap(t *testing.T) {
	st, _, inbox := newStore(t, 10)
	items := []IngestItem{
		{Kind: KindFile, Name: "f.bin", Channel: ChannelAgent, Reader: strings.NewReader("12345")}, // 5 bytes
		{Kind: KindText, Channel: ChannelAgent, Reader: strings.NewReader("ABCDEF")},               // +6 → 11 > 10
	}
	_, err := Ingest(items, classify.Peer, st, 10)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("agent mid-batch err = %v, want ErrTooLarge", err)
	}
	// The file item (item 1) is on the inbox FIFO/disk; the text trip did not remove it.
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 1 {
		t.Fatalf("inbox has %d files after a text trip on item 2, want 1 (the file stays)", len(entries))
	}
}

// errReader fails on Read so readText's error branch is exercised.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestClipboardTextReaderError drives readText's read-error path → ErrMalformed.
func TestClipboardTextReaderError(t *testing.T) {
	st, _, _ := newStore(t, 100)
	items := []IngestItem{{Kind: KindText, Channel: ChannelClipboard, Reader: errReader{}}}
	if _, err := Ingest(items, classify.Peer, st, 100); !errors.Is(err, ErrMalformed) {
		t.Fatalf("text reader error = %v, want ErrMalformed", err)
	}
}

// TestAgentTextReaderError drives readText's error path on the agent channel too.
func TestAgentTextReaderError(t *testing.T) {
	st, _, _ := newStore(t, 100)
	items := []IngestItem{{Kind: KindText, Channel: ChannelAgent, Reader: errReader{}}}
	if _, err := Ingest(items, classify.Peer, st, 100); !errors.Is(err, ErrMalformed) {
		t.Fatalf("agent text reader error = %v, want ErrMalformed", err)
	}
}

// TestNilReaderTextIsEmpty drives readText's nil-reader branch (the &""/absent
// Content-Length case): a text item with a nil Reader is the empty string, dispatched as
// one item.
func TestNilReaderTextIsEmpty(t *testing.T) {
	st, _, _ := newStore(t, 100)
	resp, err := Ingest([]IngestItem{{Kind: KindText, Channel: ChannelAgent, Reader: nil}}, classify.Peer, st, 100)
	if err != nil {
		t.Fatalf("nil-reader agent text err = %v, want nil", err)
	}
	if resp.Count != 1 {
		t.Fatalf("nil-reader text count = %d, want 1", resp.Count)
	}
}

// TestAgentUnknownKindMalformed drives ingestAgent's default (unknown kind) branch.
func TestAgentUnknownKindMalformed(t *testing.T) {
	st, _, _ := newStore(t, 100)
	items := []IngestItem{{Kind: "audio", Channel: ChannelAgent, Reader: strings.NewReader("x")}}
	if _, err := Ingest(items, classify.Peer, st, 100); !errors.Is(err, ErrMalformed) {
		t.Fatalf("agent unknown kind = %v, want ErrMalformed", err)
	}
}

// TestAgentImageSaveErrorSurfaced drives ingestAgent's SaveAgentItem error branch via the
// fakeStore seam (a non-escape errno is surfaced verbatim).
func TestAgentImageSaveErrorSurfaced(t *testing.T) {
	sentinel := errors.New("input/output error")
	fs := agentFakeStore{fakeStore: fakeStore{saveErr: sentinel}}
	items := []IngestItem{{Kind: KindImage, Name: "a.png", Channel: ChannelAgent, Reader: strings.NewReader("x")}}
	if _, err := Ingest(items, classify.Peer, fs, 100); !errors.Is(err, sentinel) {
		t.Fatalf("agent save error = %v, want the verbatim errno", err)
	}
}

// agentFakeStore embeds the ingest-test fakeStore behavior for the agent save path.
type agentFakeStore struct {
	fakeStore
}

func (f agentFakeStore) SaveAgentItem(sender, name, kind, uti string, r io.Reader) (string, int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return "", 0, f.saveErr
}
