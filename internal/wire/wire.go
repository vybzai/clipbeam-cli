// Package wire defines ClipBeam's FROZEN Envelope v1 over-the-wire JSON shapes
// plus the byte-ceiling math. It is a byte-for-behavior re-implementation of the
// Swift app's Models.swift (the literal source of truth, PLAN §3.0); a Mac running
// ClipBeam.app and a box running clipbeam must interoperate over POST /clip.
//
// Optional fields are POINTERs with omitempty so Go's encoder matches Swift's
// nil-omission AND a present-but-empty string is emitted as &"" rather than dropped
// (the load-bearing &"" rule, PLAN §3.5). On decode, pointer fields tolerate both a
// missing key and an explicit JSON null.
package wire

import "encoding/json"

// Item is one wire item. kind ∈ {image,file,text}. For text, name/uti/mime/bytesB64
// are nil and text carries the UTF-8 string; for image/file, bytesB64 carries the
// base64 payload and text is nil. Mirrors Swift Models.swift Item exactly (PLAN §3.5).
type Item struct {
	Kind     string  `json:"kind"`
	Name     *string `json:"name,omitempty"`
	Uti      *string `json:"uti,omitempty"`
	Mime     *string `json:"mime,omitempty"`
	BytesB64 *string `json:"bytesB64,omitempty"`
	Text     *string `json:"text,omitempty"`
}

// Envelope is the top-level POST /clip body. Version is a bare integer that MUST
// equal 1 forever (the protocol version, decoupled from CLI SemVer, PLAN §10.8).
// Channel is an optional routing channel: nil or "clipboard" → the clipboard
// channel; "agent" → the agent inbox channel (PLAN §3.4, Models.swift:25-30).
type Envelope struct {
	Version int     `json:"version"`
	Sender  string  `json:"sender"`
	Items   []Item  `json:"items"`
	Channel *string `json:"channel,omitempty"`
}

// ClipResponse is the 200 body for POST /clip: ok, the absolute saved paths in item
// order, and the item count. Byte-shape-frozen against Swift ClipResponse.
type ClipResponse struct {
	OK    bool     `json:"ok"`
	Saved []string `json:"saved"`
	Count int      `json:"count"`
}

// HealthResponse is the GET /health 200 body. The KEY NAMES are frozen and shared
// with the Swift app; the VALUES are the CLI's own (app:"clipbeam",
// platform:"linux <kernel>") — a liveness check must test ok==true, never the
// app/platform strings (PLAN §3.3, §8.9).
type HealthResponse struct {
	OK       bool   `json:"ok"`
	App      string `json:"app"`
	Version  string `json:"version"`
	Host     string `json:"host"`
	Platform string `json:"platform"`
}

// SendRequest is the unified input to the loopback send/control surface. It mirrors
// Swift SendRequest: POST /push (bare) → {channel:"clipboard", kind:"clipboard"};
// POST /agent-send builds one from the X-ClipBeam-* headers + body (PLAN §3.4).
//
//	channel ∈ {"agent","clipboard"}
//	kind    ∈ {"file","text","clipboard"}
//	File    absolute path for kind=="file" (nil otherwise)
//	Text    raw UTF-8 body for kind=="text" (nil otherwise)
type SendRequest struct {
	Channel string
	Kind    string
	File    *string
	Text    *string
}

// AgentItem is a dequeued agent-channel item (Models.swift AgentItem). It lives in
// memory only: file/image items reference a saved path under the agent inbox; text
// items carry their text and are never written to disk. Drained one-at-a-time by
// GET /recv (PLAN §3.10, §7.5).
type AgentItem struct {
	Type   string  // "image" | "file" | "text"
	Sender string  // the envelope's sender host (informational)
	Path   *string // set for image/file (abs path in the agent inbox)
	Text   *string // set for text only (never written to disk)
}

// EmptyString returns a pointer to "" so callers can emit the present-but-empty
// &"" form (PLAN §3.5) without a local addressable variable.
func EmptyString() *string {
	s := ""
	return &s
}

// Ptr returns a pointer to s — a present, possibly-empty optional. A nil pointer is
// the absent/omitted form; Ptr("") is the present-but-empty &"" form.
func Ptr(s string) *string {
	return &s
}

// MarshalEnvelope encodes an Envelope to wire JSON. It is a thin wrapper over
// encoding/json; key order is unspecified (Swift's wire encoder does not sort keys),
// so interop tests assert semantic (decode-then-compare) equality, never raw bytes
// (PLAN §3.5).
func MarshalEnvelope(e Envelope) ([]byte, error) {
	return json.Marshal(e)
}

// UnmarshalEnvelope decodes wire JSON into an Envelope. Pointer optionals tolerate
// both a missing key and an explicit null.
func UnmarshalEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	err := json.Unmarshal(data, &e)
	return e, err
}
