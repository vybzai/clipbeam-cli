package httpd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vybzai/clipbeam-cli/internal/ingest"
	"github.com/vybzai/clipbeam-cli/internal/wire"
)

// writeError writes the frozen JSON error body {"ok":false,"error":"<reason>"} with
// the reason sanitized (\ → /, " → ', CR/LF → space) so dynamic text (paths, errno
// strings) can never break the hand-built body (PLAN §3.11). It sets an explicit
// Content-Type; net/http sets Content-Length + Connection: close (keep-alives off).
func (s *Server) writeError(w http.ResponseWriter, status int, reason string) {
	safe := sanitizeReason(reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"ok":false,"error":"` + safe + `"}`))
}

// sanitizeReason strips characters that would break the hand-built JSON error body
// (Swift Server.spec, PLAN §3.11).
func sanitizeReason(reason string) string {
	r := strings.NewReplacer(
		`\`, `/`,
		`"`, `'`,
		"\n", " ",
		"\r", " ",
	)
	return r.Replace(reason)
}

// writeBarePath writes a text/plain body with NO trailing newline — the contract
// $(clipbeam last) substitution depends on (PLAN §3.11/§8.1). An empty path writes a
// 204 with no body.
func (s *Server) writeBarePath(w http.ResponseWriter, path string) {
	if path == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(path)) // no trailing newline
}

// writeJSON marshals v and writes it as application/json with the given status. The
// explicit Content-Length + Connection: close are set by net/http (keep-alives off).
// Bodies written here (ClipResponse, the {ok,sentItems} control response) DO carry a
// JSON structure; only /last, /wait, /recv carry the bare/labeled no-trailing-NL form.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "encode error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeSentItems writes the frozen control success body {"ok":true,"sentItems":N}
// (Swift handlePush, Server.swift:735). Hand-built to match the exact key order/shape.
func (s *Server) writeSentItems(w http.ResponseWriter, n int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true,"sentItems":` + itoa64(int64(n)) + `}`))
}

// writeRecvBody writes the labeled /recv 200 body in the FIXED order with a
// colon-SPACE separator and NO trailing newline (Server.swift:767-769, PLAN §3.11):
//
//	type: <v>\nsender: <v>\n[path: <v>\n]text: <v>
//
// `type` and `sender` are always present; `path` appears only for image/file; `text`
// is ALWAYS LAST (it may contain newlines).
func (s *Server) writeRecvBody(w http.ResponseWriter, item *wire.AgentItem) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(recvBodyString(item))) // no trailing newline
}

// recvBodyString builds the labeled /recv body string (the pure core of writeRecvBody,
// shared with the interop tests): type/sender, an optional path (image/file), and text
// LAST (it may contain newlines), colon-SPACE separators, NO trailing newline.
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

// writeClipError maps an ingest/store error from the /clip path to the frozen status
// table (PLAN §3.8/§3.11): ErrTooLarge → 413 (with the bounded drain), ErrMalformed /
// the streaming-parse 400s → 400, ErrPathEscape → a GENERIC 500 (never echo the
// path), any other store error (e.g. an ENOSPC errno) → a 500 surfacing the real
// errno as a non-secret diagnostic. NEVER 502 on this surface (PLAN §3.11).
func (s *Server) writeClipError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ingest.ErrTooLarge):
		s.write413(w, r)
	case errors.Is(err, ingest.ErrPathEscape):
		s.writeError(w, http.StatusInternalServerError, "internal error")
	case errors.Is(err, ingest.ErrMalformed),
		errors.Is(err, errUnsupportedEnvelope),
		errors.Is(err, errIncompleteBody),
		errors.Is(err, errBadJSONString):
		s.writeError(w, http.StatusBadRequest, "malformed request")
	default:
		// A MaxBytesReader trip surfaces as *http.MaxBytesError → explicit 413.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.write413(w, r)
			return
		}
		// Real errno (ENOSPC, EACCES, …) relayed as a non-secret 500 diagnostic.
		s.writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// itoa64 renders a non-negative int64 without importing strconv at the call site.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
