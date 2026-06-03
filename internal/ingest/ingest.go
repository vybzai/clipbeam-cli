// Package ingest is the keystone: the single save/dispatch path for every transport
// (PLAN §3.2). Ingest takes a per-item payload SOURCE (an io.Reader, never an
// in-memory []byte) so a 50 MB item is never held whole in RAM and /clip + the CB01
// SSH frame cannot diverge. It validates the items, streams each through the store,
// applies the per-item incremental cap + channel-dependent text counting (§3.8), runs
// the traversal-proof sanitizer (§3.9, via the store's save), and dispatches by
// channel.
package ingest

import (
	"errors"
	"io"
	"os"

	"github.com/vybzai/clipbeam-cli/internal/classify"
	"github.com/vybzai/clipbeam-cli/internal/sanitize"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// Channel constants (the wire's optional routing channel, Models.swift:25-30).
const (
	// ChannelClipboard is the human clipboard channel (nil or "clipboard" → here).
	ChannelClipboard = "clipboard"
	// ChannelAgent is the agent inbox channel.
	ChannelAgent = "agent"
)

// Item kinds (Swift ItemKind raw values).
const (
	KindImage = "image"
	KindFile  = "file"
	KindText  = "text"
)

// IngestItem is one item to save, streamed. Kind ∈ {image,file,text}; for text,
// Reader yields UTF-8 bytes and no file is written (clipboard short-text carve-out,
// PLAN §3.2). Channel is "" | "clipboard" | "agent" (empty == clipboard, the wire's
// nil collapse). Name/Uti/Mime inform the sanitizer's fallback extension only.
type IngestItem struct {
	Kind    string
	Name    string
	Uti     string
	Mime    string
	Channel string
	// Reader is the streamed payload; never read whole into a []byte for image/file
	// (PLAN §3.2, §3.7). For text the bytes are the UTF-8 string.
	Reader io.Reader
}

// ErrTooLarge is the sentinel mapped to HTTP 413 (decoded sum > maxBytes, PLAN §3.8).
var ErrTooLarge = errors.New("clipbeam: payload too large")

// ErrMalformed is the sentinel mapped to HTTP 400 (e.g. a missing/invalid payload or
// an unknown item kind, PLAN §3.11).
var ErrMalformed = errors.New("clipbeam: malformed item")

// ErrPathEscape is the sentinel mapped to a generic HTTP 500 — never echoing the
// path — when a sanitized candidate would escape the save dir (PLAN §3.9, §3.11). It
// aliases sanitize.ErrPathEscape so errors.Is matches the error the store's save
// returns across the package boundary.
var ErrPathEscape = sanitize.ErrPathEscape

// Ingest is the sole save/dispatch path (PLAN §3.2). It streams each item's payload
// through the store while accumulating a running decoded sum, applies the per-item
// incremental cap (write→add→check; a trip on item N leaves items 1..N-1 on disk and
// returns ErrTooLarge — §3.8), and dispatches by channel. The clipboard short-text
// carve-out (kind:text on the clipboard channel that the store does NOT write to a
// sidecar) is neither written to disk NOR counted toward maxBytes — the verified Swift
// asymmetry (§3.8); on the agent channel ALL text IS counted. Both use UTF-8 BYTE
// counts.
//
// All items in a single Ingest call must share one channel (the envelope's channel);
// callers split per channel before invoking. src is the validated transport class
// (passed through opaquely — Tier-1 auth happens before Ingest, §6.2). maxBytes is the
// decoded cap (config.maxBytes).
//
// On success it returns ClipResponse{ok:true, saved:[abs paths in item order],
// count:len(items)} and, for the clipboard channel, has already committed last_path +
// recents and resumed any parked /wait waiter via FinishClipboard.
func Ingest(items []IngestItem, src classify.SourceClass, st store.ReceiveStore, maxBytes int) (wire.ClipResponse, error) {
	_ = src // transport class is informational here; auth is gated upstream (§6.2)

	if len(items) == 0 {
		return wire.ClipResponse{}, ErrMalformed
	}

	channel := items[0].Channel
	if channel == ChannelAgent {
		return ingestAgent(items, st, maxBytes)
	}
	return ingestClipboard(items, st, maxBytes)
}

// ingestClipboard implements receiveClipboard (PLAN §7.2, Swift performReceive). It
// writes image/file items to the save dir (counted), and text items to a .txt sidecar
// ONLY when the store decides to (saveTextToDisk OR utf8 > longTextThreshold) — that
// sidecar text is counted, while short text is the uncounted carve-out (§3.8).
func ingestClipboard(items []IngestItem, st store.ReceiveStore, maxBytes int) (wire.ClipResponse, error) {
	var savedPaths []string
	var total int64

	for _, it := range items {
		switch it.Kind {
		case KindImage, KindFile:
			path, n, err := st.SaveClipboardItem(it.Name, it.Kind, it.Uti, it.Reader)
			if err != nil {
				return wire.ClipResponse{}, classifyStoreErr(err)
			}
			// write → add → check (per-item incremental cap, §3.8). The store streams
			// the payload before its byte count is known, so the tripping item N is
			// momentarily on disk; remove it so the observable post-413 state matches
			// Swift's add→check→save (items 1..N-1 on disk, N absent — §3.8).
			total += n
			if total > int64(maxBytes) {
				_ = os.Remove(path)
				return wire.ClipResponse{}, ErrTooLarge
			}
			savedPaths = append(savedPaths, path)

		case KindText:
			text, err := readText(it.Reader)
			if err != nil {
				return wire.ClipResponse{}, ErrMalformed
			}
			path, wrote, n, err := st.SaveClipboardText(text)
			if err != nil {
				return wire.ClipResponse{}, classifyStoreErr(err)
			}
			if wrote {
				// Sidecar branch: count its bytes (Swift Clipboard.swift:249-251).
				total += n
				if total > int64(maxBytes) {
					_ = os.Remove(path)
					return wire.ClipResponse{}, ErrTooLarge
				}
				savedPaths = append(savedPaths, path)
			}
			// else: short-text carve-out — NOT written, NOT counted (§3.8).

		default:
			return wire.ClipResponse{}, ErrMalformed
		}
	}

	if err := st.FinishClipboard(senderOf(items), savedPaths); err != nil {
		return wire.ClipResponse{}, classifyStoreErr(err)
	}

	return wire.ClipResponse{OK: true, Saved: nonNil(savedPaths), Count: len(items)}, nil
}

// ingestAgent implements receiveAgent (PLAN §7.5, Swift performReceiveAgent). Every
// item is counted (including text — the asymmetry vs the clipboard channel, §3.8);
// image/file items are saved to the agent inbox and enqueued by path, text is enqueued
// in memory only and never written to disk.
func ingestAgent(items []IngestItem, st store.ReceiveStore, maxBytes int) (wire.ClipResponse, error) {
	var savedPaths []string
	var total int64
	sender := senderOf(items)

	for _, it := range items {
		switch it.Kind {
		case KindImage, KindFile:
			path, n, err := st.SaveAgentItem(sender, it.Name, it.Kind, it.Uti, it.Reader)
			if err != nil {
				return wire.ClipResponse{}, classifyStoreErr(err)
			}
			// write → add → check (§3.8). Remove the tripping item N's file so the
			// observable on-disk state matches Swift (items 1..N-1 in the inbox, N
			// absent). The store enqueues per item (the frozen ReceiveStore contract),
			// so earlier items are already on the FIFO; a 413 mid-agent-batch is a rare
			// edge and the inbox files are the durable deliverable.
			total += n
			if total > int64(maxBytes) {
				_ = os.Remove(path)
				return wire.ClipResponse{}, ErrTooLarge
			}
			savedPaths = append(savedPaths, path)

		case KindText:
			text, err := readText(it.Reader)
			if err != nil {
				return wire.ClipResponse{}, ErrMalformed
			}
			// ALL agent text is counted (Swift Clipboard.swift:317).
			total += int64(len(text))
			if total > int64(maxBytes) {
				return wire.ClipResponse{}, ErrTooLarge
			}
			if err := st.EnqueueAgentText(sender, text); err != nil {
				return wire.ClipResponse{}, classifyStoreErr(err)
			}

		default:
			return wire.ClipResponse{}, ErrMalformed
		}
	}

	return wire.ClipResponse{OK: true, Saved: nonNil(savedPaths), Count: len(items)}, nil
}

// readText drains an item's reader into a string (text items are bounded by the
// transport ceiling upstream, so this is safe). A nil reader is an empty string (the
// &"" / absent-Content-Length case, §3.4).
func readText(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// senderOf is a placeholder for the envelope sender; IngestItem does not carry it, so
// the store records "" unless a caller threads it. The httpd layer passes the sender
// to the store via a future seam; for now the agent items carry the empty sender,
// matching the informational-only role of X-ClipBeam-Sender (§3.3).
func senderOf(items []IngestItem) string {
	_ = items
	return ""
}

// classifyStoreErr maps a store error to the ingest sentinels. A path-escape stays
// generic (ErrPathEscape → 500, never echo the path); anything else is surfaced
// verbatim so the httpd layer can expose the real errno (e.g. ENOSPC) as a non-secret
// 500 diagnostic (§3.11).
func classifyStoreErr(err error) error {
	if errors.Is(err, sanitize.ErrPathEscape) {
		return ErrPathEscape
	}
	return err
}

// nonNil returns s, or an empty (non-nil) slice when s is nil, so ClipResponse.saved
// marshals as [] rather than null (the Swift array is never null).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
