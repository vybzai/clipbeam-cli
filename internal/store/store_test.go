package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore builds a store over temp dirs with the given text policy.
func newTestStore(t *testing.T, saveTextToDisk bool, longText int) (*receiveStore, string, string) {
	t.Helper()
	base := t.TempDir()
	saveDir := filepath.Join(base, "save")
	inbox := filepath.Join(base, "agent-inbox")
	cfg := StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     inbox,
		LastPathFile:      filepath.Join(base, "last_path"),
		RecentsFile:       filepath.Join(base, "recents.json"),
		SaveTextToDisk:    saveTextToDisk,
		LongTextThreshold: longText,
		MaxBytes:          52_428_800,
	}
	return newConcrete(cfg), saveDir, inbox
}

// TestSaveClipboardItemDiskAndMode asserts an image/file item is written to the save
// dir at mode 0600 with the exact payload, and the path is absolute and under saveDir.
func TestSaveClipboardItemDiskAndMode(t *testing.T) {
	s, saveDir, _ := newTestStore(t, false, 8192)
	payload := []byte("PNGDATA\x00\x01binary")
	path, n, err := s.SaveClipboardItem("shot.png", "image", "public.png", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("SaveClipboardItem: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("written = %d, want %d", n, len(payload))
	}
	if filepath.Dir(path) != mustEval(t, saveDir) && filepath.Dir(path) != saveDir {
		t.Fatalf("path %q not under saveDir %q", path, saveDir)
	}
	if filepath.Base(path) != "shot.png" {
		t.Fatalf("leaf = %q, want shot.png", filepath.Base(path))
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %q", got)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestCollisionSuffix asserts a second save of the same leaf gets " (2)" and never
// overwrites the first (§3.9 collision style).
func TestCollisionSuffix(t *testing.T) {
	s, _, _ := newTestStore(t, false, 8192)
	p1, _, err := s.SaveClipboardItem("a.png", "image", "public.png", strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := s.SaveClipboardItem("a.png", "image", "public.png", strings.NewReader("second"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p1) != "a.png" {
		t.Fatalf("first leaf = %q, want a.png", filepath.Base(p1))
	}
	if filepath.Base(p2) != "a (2).png" {
		t.Fatalf("second leaf = %q, want 'a (2).png'", filepath.Base(p2))
	}
	if b, _ := os.ReadFile(p1); string(b) != "first" {
		t.Fatalf("first file overwritten: %q", b)
	}
	if b, _ := os.ReadFile(p2); string(b) != "second" {
		t.Fatalf("second file wrong content: %q", b)
	}
}

// TestSaveClipboardTextSidecarPolicy asserts the carve-out: short text writes nothing
// (and returns wrote=false), while text over the threshold (or saveTextToDisk) writes
// a .txt sidecar (§3.8, §7.2).
func TestSaveClipboardTextSidecarPolicy(t *testing.T) {
	// Threshold 10: 5-byte text is short → no sidecar.
	s, saveDir, _ := newTestStore(t, false, 10)
	path, wrote, n, err := s.SaveClipboardText("hello")
	if err != nil {
		t.Fatal(err)
	}
	if wrote || path != "" || n != 0 {
		t.Fatalf("short text wrote a sidecar: path=%q wrote=%v n=%d", path, wrote, n)
	}
	entries, _ := os.ReadDir(saveDir)
	if len(entries) != 0 {
		t.Fatalf("short text left %d files on disk, want 0", len(entries))
	}

	// Over threshold → sidecar written, counted.
	long := strings.Repeat("x", 11)
	path, wrote, n, err = s.SaveClipboardText(long)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || path == "" || n != int64(len(long)) {
		t.Fatalf("long text sidecar wrong: path=%q wrote=%v n=%d", path, wrote, n)
	}
	if !strings.HasSuffix(path, ".txt") {
		t.Fatalf("sidecar %q is not a .txt", path)
	}
	if b, _ := os.ReadFile(path); string(b) != long {
		t.Fatalf("sidecar content = %q, want %q", b, long)
	}

	// saveTextToDisk forces a sidecar even for short text.
	s2, _, _ := newTestStore(t, true, 8192)
	_, wrote2, _, err := s2.SaveClipboardText("hi")
	if err != nil {
		t.Fatal(err)
	}
	if !wrote2 {
		t.Fatal("saveTextToDisk=true should force a sidecar for short text")
	}
}

// TestLastPathNoTrailingNewline asserts last_path is the raw path bytes with NO
// trailing newline (the shim substitution depends on it, §7.2/§8.1).
func TestLastPathNoTrailingNewline(t *testing.T) {
	s, _, _ := newTestStore(t, false, 8192)
	p1, _, _ := s.SaveClipboardItem("one.png", "image", "public.png", strings.NewReader("1"))
	p2, _, _ := s.SaveClipboardItem("two.png", "image", "public.png", strings.NewReader("2"))
	if err := s.FinishClipboard("peer", []string{p1, p2}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.cfg.LastPathFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != p2 {
		t.Fatalf("last_path = %q, want %q (no trailing newline)", raw, p2)
	}
	if strings.HasSuffix(string(raw), "\n") {
		t.Fatal("last_path has a trailing newline (banned)")
	}
	if lp, ok := s.LastPath(); !ok || lp != p2 {
		t.Fatalf("LastPath() = %q,%v want %q,true", lp, ok, p2)
	}
	fi, _ := os.Stat(s.cfg.LastPathFile)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("last_path mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestLastPathPersistsAcrossRestart asserts a new store over the same paths reloads
// last_path from disk (GET /last survives a restart).
func TestLastPathPersistsAcrossRestart(t *testing.T) {
	s, _, _ := newTestStore(t, false, 8192)
	p, _, _ := s.SaveClipboardItem("x.png", "image", "public.png", strings.NewReader("z"))
	_ = s.FinishClipboard("peer", []string{p})

	s2 := newConcrete(s.cfg)
	if lp, ok := s2.LastPath(); !ok || lp != p {
		t.Fatalf("reloaded LastPath() = %q,%v want %q,true", lp, ok, p)
	}
}

// TestRecentsRingAndMode asserts recents.json keeps the last 20, is 0600, and is
// corruption-tolerant (a garbage file resets rather than erroring).
func TestRecentsRingAndMode(t *testing.T) {
	s, _, _ := newTestStore(t, false, 8192)

	// Append 25 distinct paths via FinishClipboard (one path each).
	for i := 0; i < 25; i++ {
		p := filepath.Join(s.cfg.SaveDir, "f"+itoaT(i)+".png")
		_ = s.FinishClipboard("peer", []string{p})
	}
	rec := loadRecents(s.cfg.RecentsFile)
	if len(rec) != recentsCap {
		t.Fatalf("recents len = %d, want %d", len(rec), recentsCap)
	}
	// The ring keeps the LAST 20 (paths f5..f24).
	if !strings.HasSuffix(rec[0].Path, "f5.png") {
		t.Fatalf("oldest kept = %q, want f5.png", rec[0].Path)
	}
	if !strings.HasSuffix(rec[len(rec)-1].Path, "f24.png") {
		t.Fatalf("newest = %q, want f24.png", rec[len(rec)-1].Path)
	}
	fi, _ := os.Stat(s.cfg.RecentsFile)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("recents mode = %v, want 0600", fi.Mode().Perm())
	}

	// Corruption-tolerant: clobber the file, then a new append resets cleanly.
	if err := os.WriteFile(s.cfg.RecentsFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadRecents(s.cfg.RecentsFile); got != nil {
		t.Fatalf("corrupt recents should load as nil, got %v", got)
	}
	p := filepath.Join(s.cfg.SaveDir, "after.png")
	_ = s.FinishClipboard("peer", []string{p})
	rec = loadRecents(s.cfg.RecentsFile)
	if len(rec) != 1 || !strings.HasSuffix(rec[0].Path, "after.png") {
		t.Fatalf("post-corruption recents = %v, want [after.png]", rec)
	}
	// Time is ISO8601-decodable.
	var probe []recent
	data, _ := os.ReadFile(s.cfg.RecentsFile)
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("recents not valid JSON: %v", err)
	}
	if probe[0].Time.IsZero() || time.Since(probe[0].Time) > time.Hour {
		t.Fatalf("recents time looks wrong: %v", probe[0].Time)
	}
}

// TestAgentInboxModesAndEnqueue asserts agent file/image items land in the inbox
// (dir 0700, files 0600) and are enqueued by path; text is enqueued in memory only and
// never written to disk (§7.5).
func TestAgentInboxModesAndEnqueue(t *testing.T) {
	s, _, inbox := newTestStore(t, false, 8192)

	path, n, err := s.SaveAgentItem("peer", "doc.bin", "file", "", strings.NewReader("agentbytes"))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("agentbytes")) {
		t.Fatalf("written = %d", n)
	}
	di, _ := os.Stat(inbox)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("inbox dir mode = %v, want 0700", di.Mode().Perm())
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("inbox file mode = %v, want 0600", fi.Mode().Perm())
	}

	// Agent text → in memory only, no new file in inbox.
	if err := s.EnqueueAgentText("peer", "just text"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(inbox)
	if len(entries) != 1 { // only the file item
		t.Fatalf("inbox has %d entries, want 1 (text must not hit disk)", len(entries))
	}

	// Drain FIFO: file item (with path) then text item (with text).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	it, _ := s.Recv(ctx)
	if it == nil || it.Type != "file" || it.Path == nil || *it.Path != path {
		t.Fatalf("first recv = %+v, want file with path %q", it, path)
	}
	it2, _ := s.Recv(ctx)
	if it2 == nil || it2.Type != "text" || it2.Text == nil || *it2.Text != "just text" {
		t.Fatalf("second recv = %+v, want text 'just text'", it2)
	}
}

// TestAgentImageItemType asserts an image kind enqueues type "image".
func TestAgentImageItemType(t *testing.T) {
	s, _, _ := newTestStore(t, false, 8192)
	p, _, err := s.SaveAgentItem("peer", "x.png", "image", "public.png", strings.NewReader("img"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	it, _ := s.Recv(ctx)
	if it == nil || it.Type != "image" || it.Path == nil || *it.Path != p {
		t.Fatalf("recv = %+v, want image with path %q", it, p)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

func itoaT(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
