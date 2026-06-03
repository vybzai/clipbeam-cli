package ingest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vybzai/clipbeam-cli/internal/classify"
	"github.com/vybzai/clipbeam-cli/internal/sanitize"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// newStore builds a real store over temp dirs so cap tests can assert that items
// 1..N-1 stay on disk after a trip on item N.
func newStore(t *testing.T, maxBytes int) (store.ReceiveStore, string, string) {
	t.Helper()
	base := t.TempDir()
	saveDir := filepath.Join(base, "save")
	inbox := filepath.Join(base, "agent-inbox")
	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     inbox,
		LastPathFile:      filepath.Join(base, "last_path"),
		RecentsFile:       filepath.Join(base, "recents.json"),
		SaveTextToDisk:    false,
		LongTextThreshold: 8192,
		MaxBytes:          maxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, saveDir, inbox
}

func bytesItem(name string, n int) IngestItem {
	return IngestItem{Kind: KindImage, Name: name, Uti: "public.png", Channel: ChannelClipboard, Reader: strings.NewReader(strings.Repeat("x", n))}
}

// TestPerItemIncrementalCap asserts the write→add→check ordering: with maxBytes=10 and
// three 4-byte items, item 1 (sum 4) and item 2 (sum 8) are written, item 3 (sum 12)
// trips → ErrTooLarge, and items 1..2 REMAIN on disk (§3.8 — not a whole-envelope
// pre-flight gate).
func TestPerItemIncrementalCap(t *testing.T) {
	st, saveDir, _ := newStore(t, 10)
	items := []IngestItem{
		bytesItem("a.png", 4),
		bytesItem("b.png", 4),
		bytesItem("c.png", 4),
	}
	_, err := Ingest(items, classify.Peer, st, 10)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Ingest err = %v, want ErrTooLarge", err)
	}
	entries, _ := os.ReadDir(saveDir)
	var saved []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".clipbeam-tmp-") {
			saved = append(saved, e.Name())
		}
	}
	if len(saved) != 2 {
		t.Fatalf("after 413 on item 3, %d files on disk, want 2 (items 1..2 stay): %v", len(saved), saved)
	}
}

// TestExactBoundaryNotTooLarge asserts sum == maxBytes is OK (the check is strictly >).
func TestExactBoundaryNotTooLarge(t *testing.T) {
	st, _, _ := newStore(t, 8)
	resp, err := Ingest([]IngestItem{bytesItem("a.png", 4), bytesItem("b.png", 4)}, classify.Peer, st, 8)
	if err != nil {
		t.Fatalf("Ingest at exact boundary err = %v, want nil", err)
	}
	if !resp.OK || resp.Count != 2 || len(resp.Saved) != 2 {
		t.Fatalf("resp = %+v, want ok/count2/saved2", resp)
	}
}

// TestClipboardShortTextNotCounted asserts the asymmetry: on the clipboard channel,
// short text (≤ threshold, saveTextToDisk=false) is neither written NOR counted. Here
// maxBytes is tiny (4) but a 100-byte short text rides through alongside a 4-byte
// image — the text bytes are NOT added, so no 413.
func TestClipboardShortTextNotCounted(t *testing.T) {
	st, saveDir, _ := newStore(t, 4) // threshold 8192 ≫ text len
	items := []IngestItem{
		bytesItem("a.png", 4),
		{Kind: KindText, Channel: ChannelClipboard, Reader: strings.NewReader(strings.Repeat("t", 100))},
	}
	resp, err := Ingest(items, classify.Peer, st, 4)
	if err != nil {
		t.Fatalf("clipboard short text should not be counted, got err %v", err)
	}
	// Only the image is saved; the short text wrote no sidecar.
	if len(resp.Saved) != 1 {
		t.Fatalf("saved = %v, want 1 (image only, short text no sidecar)", resp.Saved)
	}
	if resp.Count != 2 {
		t.Fatalf("count = %d, want 2 (count is item count, not saved count)", resp.Count)
	}
	var txt int
	entries, _ := os.ReadDir(saveDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			txt++
		}
	}
	if txt != 0 {
		t.Fatalf("short clipboard text wrote %d sidecars, want 0", txt)
	}
}

// TestAgentTextIsCounted asserts the OTHER side of the asymmetry: on the agent channel
// ALL text IS counted. A 100-byte agent text with maxBytes=4 trips 413.
func TestAgentTextIsCounted(t *testing.T) {
	st, _, _ := newStore(t, 4)
	items := []IngestItem{
		{Kind: KindText, Channel: ChannelAgent, Reader: strings.NewReader(strings.Repeat("t", 100))},
	}
	_, err := Ingest(items, classify.Peer, st, 4)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("agent text over cap err = %v, want ErrTooLarge", err)
	}
}

// TestAgentTextWithinCapEnqueued asserts agent text within the cap is enqueued (in
// memory) and never written to disk, and Recv yields it.
func TestAgentTextWithinCapEnqueued(t *testing.T) {
	st, _, inbox := newStore(t, 100)
	resp, err := Ingest([]IngestItem{
		{Kind: KindText, Channel: ChannelAgent, Reader: strings.NewReader("hello agent")},
	}, classify.Peer, st, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Saved) != 0 || resp.Count != 1 {
		t.Fatalf("resp = %+v, want saved:[] count:1 (text not on disk)", resp)
	}
	if _, statErr := os.Stat(inbox); statErr == nil {
		if entries, _ := os.ReadDir(inbox); len(entries) != 0 {
			t.Fatalf("agent text left %d files in inbox, want 0", len(entries))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200_000_000)
	defer cancel()
	it, _ := st.Recv(ctx)
	if it == nil || it.Type != "text" || it.Text == nil || *it.Text != "hello agent" {
		t.Fatalf("recv = %+v, want text 'hello agent'", it)
	}
}

// TestClipboardLongTextCounted asserts that when text DOES write a sidecar (over
// threshold), its bytes ARE counted on the clipboard channel.
func TestClipboardLongTextCounted(t *testing.T) {
	// threshold 5, maxBytes 5: an 11-byte text writes a sidecar (>5) and its 11 bytes
	// exceed maxBytes 5 → 413.
	base := t.TempDir()
	st, err := store.New(store.StoreConfig{
		SaveDir:           filepath.Join(base, "save"),
		AgentInboxDir:     filepath.Join(base, "inbox"),
		LastPathFile:      filepath.Join(base, "last_path"),
		RecentsFile:       filepath.Join(base, "recents.json"),
		LongTextThreshold: 5,
		MaxBytes:          5,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Ingest([]IngestItem{
		{Kind: KindText, Channel: ChannelClipboard, Reader: strings.NewReader(strings.Repeat("y", 11))},
	}, classify.Peer, st, 5)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("long clipboard text over cap err = %v, want ErrTooLarge", err)
	}
}

// TestBorderlineMultibyteAt8192 covers the §3.8 borderline: a multibyte text whose
// UTF-8 byte length is EXACTLY the longTextThreshold (8192). The threshold test is
// strictly > (Swift utf8.count > threshold), so at exactly 8192 it is SHORT — on the
// clipboard channel it writes no sidecar and is NOT counted; on the agent channel the
// same 8192 bytes ARE counted. Uses 4096 'é' runes = 8192 bytes.
func TestBorderlineMultibyteAt8192(t *testing.T) {
	const threshold = 8192
	text := strings.Repeat("é", 4096) // 2 bytes each → exactly 8192 bytes
	if len(text) != threshold {
		t.Fatalf("fixture is %d bytes, want %d", len(text), threshold)
	}

	// Clipboard: exactly-at-threshold is short → not written, not counted (maxBytes 1
	// would otherwise trip if it were counted).
	st, saveDir, _ := newStore(t, 1)
	resp, err := Ingest([]IngestItem{
		{Kind: KindText, Channel: ChannelClipboard, Reader: strings.NewReader(text)},
	}, classify.Peer, st, 1)
	if err != nil {
		t.Fatalf("clipboard 8192-byte text should be uncounted short text, err = %v", err)
	}
	if len(resp.Saved) != 0 {
		t.Fatalf("clipboard 8192-byte text wrote a sidecar: %v", resp.Saved)
	}
	if entries, _ := os.ReadDir(saveDir); len(entries) != 0 {
		t.Fatalf("clipboard 8192-byte text left %d files on disk, want 0", len(entries))
	}

	// One byte over (8193) → over threshold → sidecar written + counted → trips a
	// tiny maxBytes.
	over := text + "x"
	st2, _, _ := newStore(t, 1)
	if _, err := Ingest([]IngestItem{
		{Kind: KindText, Channel: ChannelClipboard, Reader: strings.NewReader(over)},
	}, classify.Peer, st2, 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("clipboard 8193-byte text err = %v, want ErrTooLarge (sidecar counted)", err)
	}

	// Agent: the SAME 8192-byte text IS counted regardless of threshold → trips.
	st3, _, _ := newStore(t, 1)
	if _, err := Ingest([]IngestItem{
		{Kind: KindText, Channel: ChannelAgent, Reader: strings.NewReader(text)},
	}, classify.Peer, st3, 1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("agent 8192-byte text err = %v, want ErrTooLarge (always counted)", err)
	}
}

// TestEmptyItemsMalformed asserts zero items → ErrMalformed (mapped to 400).
func TestEmptyItemsMalformed(t *testing.T) {
	st, _, _ := newStore(t, 100)
	if _, err := Ingest(nil, classify.Peer, st, 100); !errors.Is(err, ErrMalformed) {
		t.Fatalf("empty items err = %v, want ErrMalformed", err)
	}
}

// TestUnknownKindMalformed asserts an unknown kind → ErrMalformed.
func TestUnknownKindMalformed(t *testing.T) {
	st, _, _ := newStore(t, 100)
	items := []IngestItem{{Kind: "video", Channel: ChannelClipboard, Reader: strings.NewReader("x")}}
	if _, err := Ingest(items, classify.Peer, st, 100); !errors.Is(err, ErrMalformed) {
		t.Fatalf("unknown kind err = %v, want ErrMalformed", err)
	}
}

// TestDispatchSavesToCorrectChannel asserts clipboard items land in saveDir + set
// last_path, while agent items land in the inbox + never set last_path.
func TestDispatchSavesToCorrectChannel(t *testing.T) {
	st, saveDir, inbox := newStore(t, 1000)

	// Clipboard image → saveDir + last_path set.
	_, err := Ingest([]IngestItem{bytesItem("clip.png", 3)}, classify.Peer, st, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.LastPath(); !ok {
		t.Fatal("clipboard ingest did not set last_path")
	}
	if entries, _ := os.ReadDir(saveDir); len(entries) == 0 {
		t.Fatal("clipboard image not in saveDir")
	}

	// Agent image → inbox, last_path unchanged (still the clipboard one).
	before, _ := st.LastPath()
	_, err = Ingest([]IngestItem{{Kind: KindImage, Name: "ag.png", Uti: "public.png", Channel: ChannelAgent, Reader: strings.NewReader("ag")}}, classify.Peer, st, 1000)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := st.LastPath()
	if after != before {
		t.Fatalf("agent ingest changed last_path: %q → %q", before, after)
	}
	if entries, _ := os.ReadDir(inbox); len(entries) == 0 {
		t.Fatal("agent image not in inbox")
	}
}

// fakeStore is a ReceiveStore seam for error-mapping tests (write failure, escape).
type fakeStore struct {
	saveErr error
}

func (f fakeStore) SaveClipboardItem(name, kind, uti string, r io.Reader) (string, int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return "", 0, f.saveErr
}
func (f fakeStore) SaveClipboardText(text string) (string, bool, int64, error) {
	return "", true, int64(len(text)), f.saveErr
}
func (f fakeStore) FinishClipboard(sender string, savedPaths []string) error { return nil }
func (f fakeStore) SaveAgentItem(sender, name, kind, uti string, r io.Reader) (string, int64, error) {
	_, _ = io.Copy(io.Discard, r)
	return "", 0, f.saveErr
}
func (f fakeStore) EnqueueAgentText(sender, text string) error        { return nil }
func (f fakeStore) LastPath() (string, bool)                          { return "", false }
func (f fakeStore) Recv(ctx context.Context) (*wire.AgentItem, error) { return nil, nil }
func (f fakeStore) WaitForNext(ctx context.Context) (string, error)   { return "", nil }

// TestPathEscapeMapsGeneric asserts a sanitize.ErrPathEscape from the store maps to
// the generic ingest.ErrPathEscape (→ 500, never echoing the path, §3.9/§3.11).
func TestPathEscapeMapsGeneric(t *testing.T) {
	fs := fakeStore{saveErr: sanitize.ErrPathEscape}
	_, err := Ingest([]IngestItem{bytesItem("x.png", 1)}, classify.Peer, fs, 100)
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("escape err = %v, want ErrPathEscape", err)
	}
}

// TestRealErrnoSurfaced asserts a non-escape store error is surfaced verbatim so httpd
// can expose the real errno (e.g. ENOSPC) as a non-secret 500 diagnostic (§3.11).
func TestRealErrnoSurfaced(t *testing.T) {
	sentinel := errors.New("no space left on device")
	fs := fakeStore{saveErr: sentinel}
	_, err := Ingest([]IngestItem{bytesItem("x.png", 1)}, classify.Peer, fs, 100)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the verbatim store error", err)
	}
}
