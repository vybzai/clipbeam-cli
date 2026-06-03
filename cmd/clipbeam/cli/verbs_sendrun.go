package cli

import (
	"os"

	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/sshx"
)

// runMsg implements `clipbeam msg <text> [target]` (PLAN §8.7): ALWAYS the agent
// channel (kind=text, raw UTF-8). --reply-to <cid> prepends the opt-in
// [clipbeam:cid=<uuid>] token (agent channel only, §8.6).
func runMsg(o out, text, target, replyTo string) error {
	if replyTo != "" {
		text = prependCid(replyTo, text)
	}
	rt, err := resolveTarget(target)
	if err != nil {
		return err
	}
	items := []sshx.CB01Item{{
		Kind:    sshx.KindByte(ingest.KindText),
		Payload: []byte(text),
	}}
	// msg is always the agent channel (PLAN §8.7).
	outcome, err := pushCB01(o, rt, ingest.ChannelAgent, items)
	if err != nil {
		return err
	}
	return emitSendOutcome(o, outcome)
}

// runPush implements `clipbeam push [target]` (PLAN §8.7): bare POST /push semantics —
// read the LOCAL clipboard and beam it on the clipboard channel. Reading the local
// system clipboard is a platform-specific sender concern handled in the SSH/clipboard
// phase; the call shape (resolve target → dial → frame) is wired here.
func runPush(o out, target string) error {
	rt, err := resolveTarget(target)
	if err != nil {
		return err
	}
	items, err := readLocalClipboardItems()
	if err != nil {
		return err
	}
	outcome, err := pushCB01(o, rt, ingest.ChannelClipboard, items)
	if err != nil {
		return err
	}
	return emitSendOutcome(o, outcome)
}

// runSend implements `clipbeam send <file> [target] [--agent]` (PLAN §8.7): a file
// goes to the clipboard channel by default; --agent flips to the agent inbox. The
// remote saved path is relayed to stdout (symmetry with `last`).
func runSend(o out, file, target string, agent bool) error {
	if file != "-" {
		if _, err := os.Stat(file); err != nil {
			return configError("send: cannot read %q: %v", file, err)
		}
	}
	rt, err := resolveTarget(target)
	if err != nil {
		return err
	}
	items, err := fileCB01Items(file)
	if err != nil {
		return err
	}
	outcome, err := pushCB01(o, rt, channelFor(agent), items)
	if err != nil {
		return err
	}
	return emitSendOutcome(o, outcome)
}

// runShot implements `clipbeam shot [target] [--agent]` (PLAN §8.7): capture a
// screenshot, beam the PNG (clipboard channel by default; --agent flips). Capture is a
// platform-specific concern handled in the screenshot phase; the call shape is wired.
func runShot(o out, target string, agent bool) error {
	rt, err := resolveTarget(target)
	if err != nil {
		return err
	}
	items, err := captureScreenshotItems()
	if err != nil {
		return err
	}
	outcome, err := pushCB01(o, rt, channelFor(agent), items)
	if err != nil {
		return err
	}
	return emitSendOutcome(o, outcome)
}

// fileCB01Items builds the CB01 items for a file send. The raw payload is streamed from
// disk by the SSH phase's encoder; the metadata (name/kind) is set here so routing +
// filename fidelity survive the daemonless path (PLAN §5.1). For "-" (stdin) the name
// is a generic stream name.
func fileCB01Items(file string) ([]sshx.CB01Item, error) {
	name := file
	if file == "-" {
		name = "clipbeam-stdin"
	} else if i := lastSlashIdx(file); i >= 0 {
		name = file[i+1:]
	}
	payload, err := readFilePayload(file)
	if err != nil {
		return nil, err
	}
	return []sshx.CB01Item{{
		Kind:    sshx.KindByte(ingest.KindFile),
		Name:    name,
		Payload: payload,
	}}, nil
}

// readFilePayload reads the file (or stdin for "-") into memory. The SSH phase replaces
// this with a streamed CB01 encode (io.CopyN, never read-all) for large items; the
// current call shape keeps the metadata + payload seam stable.
func readFilePayload(file string) ([]byte, error) {
	if file == "-" {
		return os.ReadFile(os.Stdin.Name())
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, configError("send: read %q: %v", file, err)
	}
	return b, nil
}

// readLocalClipboardItems reads the local system clipboard into CB01 items. The
// platform clipboard read (pbpaste/xclip/wl-paste) lands in the clipboard phase; until
// then it surfaces the not-ready condition so `push` fails cleanly rather than sending
// an empty frame.
func readLocalClipboardItems() ([]sshx.CB01Item, error) {
	return nil, configError("push: local clipboard read is not available in this build")
}

// captureScreenshotItems captures a screenshot into a CB01 image item. The capture
// backend (screencapture/grim/spectacle) lands in the screenshot phase; until then it
// surfaces the not-ready condition so `shot` fails cleanly.
func captureScreenshotItems() ([]sshx.CB01Item, error) {
	return nil, configError("shot: screenshot capture is not available in this build")
}

// lastSlashIdx returns the index of the last '/' in s, or -1 (kept local to the send
// verbs so this package does not import path/filepath just for a basename).
func lastSlashIdx(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
