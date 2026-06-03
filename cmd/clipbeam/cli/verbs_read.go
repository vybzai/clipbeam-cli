package cli

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/config"
	"github.com/vybzai/clipbeam-cli/internal/store"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// loadControl resolves the config + token and builds a control client for the local
// daemon (PLAN §5.1). The token is loaded from the per-OS store; a missing token is
// tolerated here because the unix-socket path is token-free (the read verbs only need
// the token on a --listen tcp daemon).
func loadControl() (*controlClient, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, configError("%v", err)
	}
	p, err := config.Resolve()
	if err != nil {
		return nil, configError("%v", err)
	}
	token := ""
	if ts, err := config.OpenTokenStore(config.TokenStoreAuto, p); err == nil {
		if tok, ok, err := ts.Load(); err == nil && ok {
			token = tok
		}
	}
	return newControlClient(cfg, token)
}

// runLast implements `clipbeam last` (PLAN §8.1/§8.2): GET /last → the bare absolute
// path with NO trailing newline; nothing-available prints nothing and exits 0. Under
// --json it wraps the path (or null) as a one-line object WITH a newline.
func runLast(o out) error {
	c, err := loadControl()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, body, err := c.get(ctx, "/last")
	if err != nil {
		// The DEFAULT daemonless-exec setup starts NO daemon, so the control dial is
		// refused / the socket is missing. Fall back to the on-disk last_path that
		// FinishClipboard already persists (fix [E]). Only on a genuine connect failure —
		// never shadow a reachable daemon's HTTP status (preserves Mac<->Mac + Tailscale).
		if isDaemonUnreachable(err) {
			return emitLastPathFromDisk(o)
		}
		return coded(ExitUnreachable, transportErr(err))
	}
	switch status {
	case 200:
		path := string(body)
		if o.json {
			return o.emitJSON(pathJSON{Schema: schemaVersion, OK: true, Path: &path})
		}
		o.data(path) // bare path, NO trailing newline (PLAN §8.1)
		return nil
	case 204:
		return emitNothingPath(o) // nothing available — print nothing, exit 0
	default:
		return statusErr(status, body)
	}
}

// emitLastPathFromDisk reads the on-disk last_path and emits it via the SAME 200 branch
// shape as the daemon path (bare no-NL path in default mode / pathJSON in --json), else
// the nothing-available result (fix [E]). Used as the daemonless fallback for last/wait.
func emitLastPathFromDisk(o out) error {
	p, err := config.Resolve()
	if err != nil {
		return configError("%v", err)
	}
	if path, ok := store.ReadLastPath(p.LastPath); ok {
		if o.json {
			return o.emitJSON(pathJSON{Schema: schemaVersion, OK: true, Path: &path})
		}
		o.data(path) // bare path, NO trailing newline (PLAN §8.1)
		return nil
	}
	return emitNothingPath(o)
}

// isDaemonUnreachable reports whether err is a genuine "no daemon" connect failure (a
// refused TCP dial or a missing unix socket — control.go falls back to TCP when no
// socket exists, so the failure surfaces as a net dial error), as opposed to a reachable
// daemon returning an HTTP status (fix [E]). It is the gate for the on-disk fallback in
// last/wait/recv and is shared with the [F] recv disk-drain.
func isDaemonUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist)
}

// runWait implements `clipbeam wait` (PLAN §8.1): GET /wait blocks up to the server's
// fixed 120 s, then prints the bare path (no trailing newline) or, on timeout (204),
// prints nothing and exits 0. Under --json it wraps the path (null ⇒ timeout).
func runWait(o out) error {
	c, err := loadControl()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlGetTimeout)
	defer cancel()
	status, body, err := c.get(ctx, "/wait")
	if err != nil {
		if isTimeout(ctx, err) {
			return emitNothingPath(o) // a normal long-poll timeout is success (exit 0)
		}
		// Daemonless (the default exec setup): no on-disk waiter primitive exists, so do a
		// ONE-SHOT read of last_path — emit it if present, else nothing (fix [E]). True
		// long-poll /wait coalescing requires a running daemon.
		if isDaemonUnreachable(err) {
			return emitLastPathFromDisk(o)
		}
		return coded(ExitUnreachable, transportErr(err))
	}
	switch status {
	case 200:
		path := string(body)
		if o.json {
			return o.emitJSON(pathJSON{Schema: schemaVersion, OK: true, Path: &path})
		}
		o.data(path)
		return nil
	case 204:
		return emitNothingPath(o)
	default:
		return statusErr(status, body)
	}
}

// runRecv implements `clipbeam recv [--timeout N]` (PLAN §8.2): GET /recv?timeout=N
// dequeues one agent FIFO item → the labeled block (default mode) or the normalized
// --json object; 204 on timeout prints nothing (default) / {…,"item":null} (--json)
// and exits 0.
func runRecv(o out, timeout int) error {
	c, err := loadControl()
	if err != nil {
		return err
	}
	// Bound the client context to the server poll window plus the server's +15 grace.
	deadline := time.Duration(timeout)*time.Second + 20*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	status, body, err := c.get(ctx, "/recv?timeout="+itoa(timeout))
	if err != nil {
		if isTimeout(ctx, err) {
			return emitRecvTimeout(o)
		}
		// Daemonless (the default exec setup): the agent channel is disk-backed, so drain
		// the on-disk journal that `clipbeam ingest` persisted (fix [F]). One-shot: an
		// empty journal is a normal timeout (exit 0).
		if isDaemonUnreachable(err) {
			return drainAgentDiskAndEmit(o)
		}
		return coded(ExitUnreachable, transportErr(err))
	}
	switch status {
	case 200:
		item := parseRecvBody(string(body))
		if o.json {
			return o.emitJSON(recvJSON{
				Schema:  schemaVersion,
				OK:      true,
				Type:    item.Type,
				Sender:  item.Sender,
				Path:    nilIfEmpty(item.Path),
				Text:    textPtr(item),
				Channel: "agent",
				Cid:     nilIfEmpty(item.Cid),
			})
		}
		// Default mode: re-emit the labeled block VERBATIM (byte-for-byte the Swift
		// output, PLAN §8.9). The body already lacks a trailing newline.
		o.data(string(body))
		return nil
	case 204:
		return emitRecvTimeout(o)
	default:
		return statusErr(status, body)
	}
}

// drainAgentDiskAndEmit is the daemonless recv fallback (fix [F]): it opens a fresh store
// over the resolved config paths and one-shot-drains the on-disk agent journal. A drained
// item is emitted through the EXACT same code path as the daemon 200 branch — the labeled
// body is built by recvBodyString and re-emitted verbatim in default mode, and --json
// funnels that same body through parseRecvBody → recvJSON — so default and --json output
// are byte-identical to the daemon path (the frozen interop fixtures are unaffected). An
// empty journal is a normal recv timeout (exit 0).
func drainAgentDiskAndEmit(o out) error {
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
	})
	if err != nil {
		return coded(ExitGeneric, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	item, derr := st.DrainAgentDisk(ctx)
	if derr != nil {
		return coded(ExitGeneric, derr)
	}
	if item == nil {
		return emitRecvTimeout(o) // empty journal — nothing to dequeue
	}
	body := recvBodyString(item)
	if o.json {
		it := parseRecvBody(body)
		return o.emitJSON(recvJSON{
			Schema:  schemaVersion,
			OK:      true,
			Type:    it.Type,
			Sender:  it.Sender,
			Path:    nilIfEmpty(it.Path),
			Text:    textPtr(it),
			Channel: "agent",
			Cid:     nilIfEmpty(it.Cid),
		})
	}
	o.data(body) // labeled block VERBATIM, no trailing newline (byte-identical to /recv)
	return nil
}

// recvBodyString builds the labeled /recv body for a drained agent item — the SAME
// builder as httpd.recvBodyString (Server.swift:767-769, PLAN §8.2): type/sender, an
// optional path (image/file), then text LAST (it may contain newlines), colon-SPACE
// separators, NO trailing newline. Duplicated here (the httpd version is unexported in
// another package) so the daemonless disk-drain emits byte-identical output to the daemon.
func recvBodyString(item *wire.AgentItem) string {
	var b strings.Builder
	b.WriteString("type: ")
	b.WriteString(item.Type)
	b.WriteString("\nsender: ")
	b.WriteString(item.Sender)
	if item.Path != nil {
		b.WriteString("\npath: ")
		b.WriteString(*item.Path)
	}
	if item.Text != nil {
		b.WriteString("\ntext: ")
		b.WriteString(*item.Text)
	}
	return b.String()
}

// itoa renders a non-negative int as decimal without importing strconv at the call
// site (kept local so the read verbs do not pull strconv just for the query param).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// pathJSON is the --json shape for last/wait (PLAN §8.2): {"schema","ok","path":<abs|null>}.
type pathJSON struct {
	Schema string  `json:"schema"`
	OK     bool    `json:"ok"`
	Path   *string `json:"path"`
}

// recvJSON is the --json shape for recv (PLAN §8.2).
type recvJSON struct {
	Schema  string  `json:"schema"`
	OK      bool    `json:"ok"`
	Type    string  `json:"type"`
	Sender  string  `json:"sender"`
	Path    *string `json:"path"`
	Text    *string `json:"text"`
	Channel string  `json:"channel"`
	Cid     *string `json:"cid"`
}

// recvTimeoutJSON is the --json timeout shape for recv (PLAN §8.2): item:null.
type recvTimeoutJSON struct {
	Schema string `json:"schema"`
	OK     bool   `json:"ok"`
	Item   *int   `json:"item"` // always null
}

// emitNothingPath emits the "nothing available" result for last/wait (PLAN §8.1/§8.2):
// in default mode prints NOTHING and exits 0; under --json emits {…,"path":null}.
func emitNothingPath(o out) error {
	if o.json {
		return o.emitJSON(pathJSON{Schema: schemaVersion, OK: true, Path: nil})
	}
	return nil // print nothing, exit 0
}

// emitRecvTimeout emits the recv timeout result (PLAN §8.2): default prints nothing;
// --json emits {…,"item":null}.
func emitRecvTimeout(o out) error {
	if o.json {
		return o.emitJSON(recvTimeoutJSON{Schema: schemaVersion, OK: true, Item: nil})
	}
	return nil
}

// nilIfEmpty returns nil for an empty string so the JSON field marshals as null, else
// a pointer to the value (PLAN §8.2: path/text/cid are <value|null>).
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// textPtr returns the text pointer for a recv item: nil when no text: line was present
// (image/file items), else a pointer to the text (which may be the empty string for a
// text:"" item, distinct from null — PLAN §8.2/§8.6).
func textPtr(it recvItem) *string {
	if !it.HasText {
		return nil
	}
	t := it.Text
	return &t
}

// isTimeout reports whether err is a context deadline/cancel from the bounding ctx (a
// normal long-poll timeout, exit 0 — PLAN §8.3), not a real transport failure.
func isTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
