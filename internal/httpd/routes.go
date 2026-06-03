package httpd

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// recvDefaultTimeout is the GET /recv default poll window in seconds (PLAN §3.3/§3.10).
const recvDefaultTimeout = 120

// recvDeadlineGrace is the server-side connection-deadline slack over the poll window
// (timeout+15, PLAN §3.10) so a just-dequeued item is never torn down before it is
// written.
const recvDeadlineGrace = 15 * time.Second

// waitTimeout is the FIXED GET /wait long-poll window (no query param, PLAN §3.10).
const waitTimeout = 120 * time.Second

// channelHeader / kindHeader / fileHeader are the frozen /agent-send control headers
// (PLAN §3.4). Channel defaults to clipboard, Kind defaults to clipboard.
const (
	channelHeader = "X-ClipBeam-Channel"
	kindHeader    = "X-ClipBeam-Kind"
	fileHeader    = "X-ClipBeam-File"
)

// routes registers the full frozen route table (PLAN §3.3) on mux. The mux root
// distinguishes 405 (known path, wrong method) from 404 (unknown path); each handler
// applies its gate in the mandated order (class → token / unix-socket-owner) before
// doing work. All routes are method-multiplexed inside one registration per path so a
// known-path wrong-method returns 405 not 404.
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("/health", s.handleHealth)        // GET, any source
	mux.HandleFunc("/clip", s.handleClip)            // POST, .peer + token (clip.go)
	mux.HandleFunc("/push", s.handlePush)            // POST, control gate
	mux.HandleFunc("/agent-send", s.handleAgentSend) // POST, control gate
	mux.HandleFunc("/recv", s.handleRecv)            // GET, control gate, long-poll
	mux.HandleFunc("/last", s.handleLast)            // GET, control gate
	mux.HandleFunc("/wait", s.handleWait)            // GET, control gate, 120 s
	// Unknown paths fall through to mux's default 404.
}

// handleHealth serves GET /health to ANY source (no gate, PLAN §3.3): 200 JSON
// {ok,app,version,host,platform}. The KEY NAMES are frozen and shared with the Swift
// app; the VALUES are the CLI's own — app:"clipbeam", platform:"<goos> <kernel>" — a
// liveness check must test ok==true, never the app/platform strings (PLAN §8.9).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	host, _ := os.Hostname()
	s.writeJSON(w, http.StatusOK, wire.HealthResponse{
		OK:       true,
		App:      "clipbeam",
		Version:  s.opts.AppVersion,
		Host:     host,
		Platform: runtime.GOOS,
	})
}

// handlePush serves POST /push (PLAN §3.4): bare-only → a clipboard-channel clipboard
// read. The ?file=ABS back-compat form is recognized server-side (Server.swift:495)
// but the Go CLI never emits it. The clipboard read itself is a SEND-side concern
// (read the local clipboard); on a headless receiver there is no clipboard to read, so
// this returns the frozen {ok,sentItems:0} control body — the receiver has nothing to
// push from a non-existent clipboard. (A Mac CLI sender drives /push against a peer;
// this handler is the receive-side acknowledgement.)
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if status, reason := s.controlAuthorized(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}
	// Bare /push reads the local clipboard; ?file=ABS is the back-compat file form.
	// On this headless receiver there is no system clipboard to read (PLAN §7.2/§7.4),
	// so a bare /push has nothing to enqueue → sentItems:0. A ?file=ABS pushes the
	// named file into the agent inbox so it is retrievable by recv (the only useful
	// receive-side action for the back-compat form).
	if file := r.URL.Query().Get("file"); file != "" {
		n, err := s.pushFile(r.Context(), file)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.writeSentItems(w, n)
		return
	}
	s.writeSentItems(w, 0)
}

// pushFile ingests a local absolute file path into the clipboard channel (the
// back-compat /push?file=ABS behavior). It opens the file and runs it through the
// single Ingest path so the sanitizer + atomic write + last_path/recents all apply.
func (s *Server) pushFile(ctx context.Context, path string) (int, error) {
	_ = ctx
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	name := path
	if i := lastSlash(path); i >= 0 {
		name = path[i+1:]
	}
	items := []ingest.IngestItem{{
		Kind:    ingest.KindFile,
		Name:    name,
		Channel: ingest.ChannelClipboard,
		Reader:  f,
	}}
	resp, err := ingest.Ingest(items, s.opts.classOf(), s.opts.Store, s.maxBytes())
	if err != nil {
		return 0, err
	}
	return resp.Count, nil
}

// handleAgentSend serves POST /agent-send (PLAN §3.4): X-ClipBeam-Channel (default
// clipboard) + X-ClipBeam-Kind (default clipboard) drive routing.
//
//   - kind=file → absolute path VERBATIM in X-ClipBeam-File (spaces OK, no newlines);
//     NO body. The file is ingested into the channel.
//   - kind=text → raw UTF-8 in the BODY; absent Content-Length ⇒ empty text dispatched
//     immediately (text:"", Server.swift:566); body bounded by the hard ceiling.
//   - kind=clipboard → no body; nothing to enqueue on a headless receiver → sentItems:0.
func (s *Server) handleAgentSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if status, reason := s.controlAuthorized(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}

	channel := headerOr(r, channelHeader, ingest.ChannelClipboard)
	kind := headerOr(r, kindHeader, "clipboard")

	switch kind {
	case ingest.KindText:
		s.agentSendText(w, r, channel)
	case ingest.KindFile:
		file := r.Header.Get(fileHeader)
		s.agentSendFile(w, r, channel, file)
	default: // "clipboard"
		// No body; nothing to enqueue on a headless receiver (PLAN §7.4).
		s.writeSentItems(w, 0)
	}
}

// agentSendText reads the raw UTF-8 body (bounded by the hard ceiling) and ingests it
// as a text item on the requested channel. An absent Content-Length ⇒ empty text
// dispatched immediately (Server.swift:564-568).
func (s *Server) agentSendText(w http.ResponseWriter, r *http.Request, channel string) {
	ceiling := wire.HardCeiling(s.maxBytes())
	if r.ContentLength > int64(ceiling) {
		s.write413(w, r)
		return
	}
	var text string
	if r.ContentLength != 0 && r.Body != nil {
		body := http.MaxBytesReader(w, r.Body, int64(ceiling))
		b, err := readAllBounded(body)
		if err != nil {
			var mbe *http.MaxBytesError
			if asMaxBytes(err, &mbe) {
				s.write413(w, r)
				return
			}
			s.writeError(w, http.StatusBadRequest, "read error")
			return
		}
		text = string(b)
	}
	items := []ingest.IngestItem{{
		Kind:    ingest.KindText,
		Channel: channel,
		Reader:  stringReader(text),
	}}
	resp, err := ingest.Ingest(items, s.opts.classOf(), s.opts.Store, s.maxBytes())
	if err != nil {
		s.writeClipError(w, r, err)
		return
	}
	s.writeSentItems(w, resp.Count)
}

// agentSendFile ingests the absolute file named verbatim in X-ClipBeam-File into the
// requested channel (no body). An empty/missing file header is a malformed request.
func (s *Server) agentSendFile(w http.ResponseWriter, r *http.Request, channel, file string) {
	if file == "" {
		s.writeError(w, http.StatusBadRequest, "missing file header")
		return
	}
	f, err := os.Open(file)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	name := file
	if i := lastSlash(file); i >= 0 {
		name = file[i+1:]
	}
	items := []ingest.IngestItem{{
		Kind:    ingest.KindFile,
		Name:    name,
		Channel: channel,
		Reader:  f,
	}}
	resp, err := ingest.Ingest(items, s.opts.classOf(), s.opts.Store, s.maxBytes())
	if err != nil {
		s.writeClipError(w, r, err)
		return
	}
	s.writeSentItems(w, resp.Count)
}

// handleRecv serves GET /recv?timeout=N (default 120 s): dequeue one agent FIFO item
// → 200 labeled text/plain (type:/sender:/optional path:/text: last, colon-SPACE) or
// 204 on timeout. The server-side deadline is timeout+15 (PLAN §3.10).
func (s *Server) handleRecv(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if status, reason := s.controlAuthorized(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}
	timeout := recvDefaultTimeout
	if q := r.URL.Query().Get("timeout"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			timeout = n
		}
	}
	// Park up to timeout, but bound the wait to the request context too (a client
	// disconnect cancels it). The server-side connection deadline grace (+15) is the
	// Swift connection-deadline analogue; net/http manages the write deadline itself,
	// so the context deadline IS the poll window here.
	deadline := time.Duration(timeout)*time.Second + recvDeadlineGrace
	ctx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()

	// Use the requested poll window for the actual park; the +15 only guards teardown.
	pollCtx, pollCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer pollCancel()

	item, err := s.opts.Store.Recv(pollCtx)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if item == nil {
		w.WriteHeader(http.StatusNoContent) // timeout/empty
		return
	}
	s.writeRecvBody(w, item)
}

// handleLast serves GET /last: 200 absolute path (NO trailing newline) or 204
// (PLAN §3.3/§7.7).
func (s *Server) handleLast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if status, reason := s.controlAuthorized(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}
	if path, ok := s.opts.Store.LastPath(); ok {
		s.writeBarePath(w, path)
		return
	}
	s.writeBarePath(w, "") // 204 when nothing available
}

// handleWait serves GET /wait: FIXED 120 s long-poll (no query param) → 200 path (NO
// trailing newline) or 204 (PLAN §3.3/§3.10).
func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if status, reason := s.controlAuthorized(r); status != 0 {
		s.writeError(w, status, reason)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), waitTimeout)
	defer cancel()
	path, err := s.opts.Store.WaitForNext(ctx)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeBarePath(w, path) // empty path ⇒ 204 (timeout)
}

// headerOr returns the first value of header key, or def when absent/empty.
func headerOr(r *http.Request, key, def string) string {
	if v := r.Header.Get(key); v != "" {
		return v
	}
	return def
}

// lastSlash returns the index of the last '/' in s, or -1.
func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
