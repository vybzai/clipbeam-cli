package cli

import (
	"bufio"
	"io"
	"os"

	"github.com/vybzai/clipbeam-cli/internal/classify"
	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
	"github.com/vybzai/clipbeam-cli/internal/store"
)

// runIngest implements the hidden `clipbeam ingest` (PLAN §5.1): the remote-side
// endpoint of the daemonless-exec push. It reads a raw-bytes CB01 frame from stdin,
// decodes it into per-item streamed readers, runs the SINGLE Ingest path (sanitize →
// atomic write → last_path/recents or agent-inbox), and prints the saved ABSOLUTE
// path(s) to stdout — one per line, NO trailing newline on the last (so the laptop
// sender can relay them as the deliverable, symmetric with `last`). Diagnostics go to
// stderr; exit 0 on success.
//
// CB01 is raw bytes, so raw==decoded: the cap is decoded-sum ≤ maxBytes DIRECTLY (NOT
// the base64-inflated /clip ceiling, PLAN §5.1).
func runIngest(o out) error {
	cfg, err := config.Load()
	if err != nil {
		return configError("%v", err)
	}
	p, err := config.Resolve()
	if err != nil {
		return configError("%v", err)
	}
	saveDir, err := config.ResolvedSaveDir(cfg)
	if err != nil {
		return configError("%v", err)
	}
	st, err := store.New(store.StoreConfig{
		SaveDir:           saveDir,
		AgentInboxDir:     p.AgentInbox,
		LastPathFile:      p.LastPath,
		RecentsFile:       p.Recents,
		SaveTextToDisk:    cfg.SaveTextToDisk,
		LongTextThreshold: cfg.LongTextThreshold,
		MaxBytes:          maxBytesOf(cfg),
		// `clipbeam ingest` is the ONE-SHOT daemonless writer: its in-memory FIFO dies with
		// this process, so an agent item must be journaled to disk for a later daemonless
		// `clipbeam recv` (a different process) to drain it (fix [F]). This is the ONLY store
		// construction that opts into the journal; the serve daemon does NOT (it is the live
		// /recv consumer of its own in-memory FIFO — journaling there would pile up plaintext
		// (H1) and double-deliver (H2)).
		JournalAgentItems: true,
	})
	if err != nil {
		return coded(ExitGeneric, err)
	}

	// Decode the CB01 frame header; payloads are streamed per item (NEVER read whole).
	channelByte, items, err := sshx.DecodeCB01(bufio.NewReader(os.Stdin))
	if err != nil {
		// A stubbed DecodeCB01 returns errNotImplemented until the SSH phase lands; a
		// genuine bad frame returns ErrBadFrame. Both surface as a generic failure here
		// (the malformed-frame case is a client/protocol error, exit 1).
		return coded(ExitGeneric, err)
	}

	channel := sshx.ChannelString(channelByte)
	ingestItems := make([]ingest.IngestItem, 0, len(items))
	for _, it := range items {
		ingestItems = append(ingestItems, ingest.IngestItem{
			Kind:    sshx.KindString(it.Kind),
			Name:    it.Name,
			Uti:     it.Uti,
			Mime:    it.Mime,
			Channel: channel,
			Reader:  it.Payload,
		})
	}

	resp, err := ingest.Ingest(ingestItems, classify.Peer, st, maxBytesOf(cfg))
	if err != nil {
		return ingestErrToCoded(err)
	}

	o.diag("ingest: saved %d item(s) on channel %q", resp.Count, channel)
	writePathsNoTrailingNewline(o.stdout, resp.Saved)
	return nil
}

// writePathsNoTrailingNewline writes one path per line with NO trailing newline after
// the LAST path (PLAN §5.1) so the sender relays the deliverable cleanly. An empty set
// writes nothing.
func writePathsNoTrailingNewline(w io.Writer, paths []string) {
	for i, p := range paths {
		if i > 0 {
			_, _ = io.WriteString(w, "\n")
		}
		_, _ = io.WriteString(w, p)
	}
}

// maxBytesOf returns the decoded-payload cap (config.maxBytes), defaulting when the
// config carries a non-positive value (PLAN §3.8).
func maxBytesOf(cfg config.Config) int {
	if cfg.MaxBytes > 0 {
		return cfg.MaxBytes
	}
	return defaultMaxBytes
}

// ingestErrToCoded maps an Ingest sentinel to the unified exit table (PLAN §8.3):
// ErrTooLarge → 7, ErrMalformed → 2 (a bad client frame is a usage/protocol error on
// the ingest path), ErrPathEscape / any store errno → 9 (remote I/O).
func ingestErrToCoded(err error) error {
	switch {
	case isErr(err, ingest.ErrTooLarge):
		return coded(ExitTooLarge, err)
	case isErr(err, ingest.ErrMalformed):
		return coded(ExitUsage, err)
	default:
		return coded(ExitRemoteIO, err)
	}
}
