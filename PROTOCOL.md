# ClipBeam Wire Protocol — Envelope v1

This document is a **derived, human-readable description** of ClipBeam's frozen
Envelope v1 wire format. It is **not** the authority.

## Source of truth & precedence

> **The Swift source and the captured golden fixtures are authoritative.**

The literal source of truth is the Swift implementation in the (private) Mac-app
repository —
`Sources/ClipBeam/{Models,Config,HTTPCore,Server,Sender,Clipboard}.swift`
(`Models.swift` is annotated "FROZEN … literal source of truth") — **plus the
golden interop fixtures captured from instrumented real runs of the shipped app**,
vendored in this repository under `testdata/interop/`.

**Precedence rule:** if this document's prose and the Swift source ever disagree,
the **Swift source + the captured golden fixtures win**. The CI-enforced contract
is the golden fixtures (decode-then-compare), never this prose. When the frozen
protocol is clarified, `PROTOCOL.md` is regenerated from the source/fixtures — never
edited freehand to introduce a new behavior.

A Mac running `ClipBeam.app` and a box running `clipbeam` must interoperate in both
directions over `POST /clip`. The wire is **byte-for-behavior** compatible; the CLI
is a re-implementation, not a redesign.

## Frozen identifiers (a binary rename never touches these)

These are part of the observable contract. CI fails if the header casing or the
`/health` key names change.

- HTTP headers: `X-ClipBeam-Token`, `X-ClipBeam-Sender`, `X-ClipBeam-Channel`,
  `X-ClipBeam-Kind`, `X-ClipBeam-File` (parsed case-insensitively; the sender emits
  these exact casings so the peer's logs/redaction match).
- `/health` JSON key names: `ok`, `app`, `version`, `host`, `platform` (the **key
  names** are frozen; the **values** are each node's own).
- macOS Keychain item: service `com.sani.clipbeam`, account `shared-token`.

## The Envelope (POST /clip body)

```jsonc
{
  "version": 1,                 // bare integer, MUST equal 1 forever
  "sender": "<hostname>",       // informational only; NEVER gates anything
  "items": [ <Item>, ... ],     // length >= 1
  "channel": "agent"            // OPTIONAL; absent or "clipboard" => clipboard channel
}
```

`version` is the **protocol** version. It is the integer `1` forever and is fully
decoupled from the CLI's SemVer release version (which is a separate field reported
by `clipbeam version` and in `/health`). It is never bumped to match a release.

### Item

```jsonc
{
  "kind": "image|file|text",    // required
  "name": "screenshot.png",     // optional
  "uti":  "public.png",         // optional
  "mime": "image/png",          // optional
  "bytesB64": "<base64>",       // optional; image/file payload (see encoding rules)
  "text": "hello"               // optional; UTF-8 text payload
}
```

For `kind:text`, `bytesB64` is absent and `text` carries the UTF-8 string. For
`kind:image`/`kind:file`, `bytesB64` carries the payload and `text` is absent.

### Optional-field encoding (the `&""` rule — load-bearing for interop)

Optional fields are **omitted when absent** (no `"name":null` on the wire) **and
emitted as an empty string when present-but-empty**:

- **absent** = the key is omitted entirely.
- **present-but-empty** = the key is emitted with value `""`.

Swift encodes a text Item with `text == ""` as `{"kind":"text","text":""}` (an empty
string is not nil, so it is emitted). An empty `POST /agent-send` body produces
exactly this. A naive encoder that drops `""` would diverge, so the CLI uses pointer
optionals: `nil` → omitted, `&""` → emitted as `""`. On decode, both a missing key
and an explicit `null` are tolerated.

Interop fixtures assert **semantic equality (decode-then-compare)**, never raw
bytes — the Swift wire encoder uses unspecified key order (only `config.json` sorts
keys).

### base64 encoding rule

`bytesB64` must be **unwrapped, standard** base64
(`base64.StdEncoding`, no MIME/PEM line wrapping). The Mac receiver's
`Data(base64Encoded:)` uses default options that **reject** any whitespace or
newline and would return `400`.

## The 200 response (ClipResponse)

```jsonc
{ "ok": true, "saved": ["<abs path>", ...], "count": 2 }
```

`saved` carries the receiver's chosen absolute paths in item order. The save
location is **not** wire-constrained — the sender treats `saved` as opaque and only
displays it.

## Byte ceilings

- **Decoded cap** = `config.maxBytes` (default `52428800` = 50 MB).
- **Raw-wire hard ceiling** (base64-JSON `/clip` only) = `maxBytes*4/3 + 64*1024`
  with **integer, multiply-first** arithmetic:
  `(52428800*4)/3 + 65536 = 69905066 + 65536 = 69970602` bytes. Writing it as
  `maxBytes/3*4` or with floats changes the truncation and would accept/reject a
  payload the Mac app would reject/accept. A unit test asserts the literal
  `69970602`.

Per-item enforcement is **incremental, not a whole-envelope pre-flight**: each item
is written, its decoded bytes added to a running sum, then checked; a trip on item N
leaves items `1..N-1` already on disk and returns `413`.

**Channel-dependent text counting (a verified asymmetry):** on the **clipboard**
channel, short text (`<= longTextThreshold`, default 8192 bytes, with
`saveTextToDisk=false`) is **not** counted toward `maxBytes` and is **not** written
to disk. On the **agent** channel, **all** text **is** counted. Both use the UTF-8
**byte** count, not the rune count.

## Routes

| Method | Path | Success | Notes |
|---|---|---|---|
| GET | `/health` | 200 JSON `{ok,app,version,host,platform}` | key names frozen; values node-specific |
| POST | `/clip` | 200 `ClipResponse` | the peer wire; base64-in-JSON |
| POST | `/push` | 200 `{ok,sentItems:N}` | bare only; reads local clipboard |
| POST | `/agent-send` | 200 `{ok,sentItems:N}` | headers drive channel/kind; body = text |
| GET | `/recv?timeout=N` | 200 labeled text/plain OR 204 | default 120 s |
| GET | `/last` | 200 path (NO trailing newline) OR 204 | |
| GET | `/wait` | 200 path (NO trailing newline) OR 204 | fixed 120 s long-poll |

`/push`, `/agent-send`, `/recv`, `/last`, `/wait` are **control** endpoints — local
only, never exposed to a peer (see `SECURITY.md`).

### `/agent-send` headers

- `X-ClipBeam-Channel` (default `clipboard`), `X-ClipBeam-Kind` (default `clipboard`).
- `kind=file` → the absolute path **verbatim** in `X-ClipBeam-File` (no URL-encoding;
  spaces OK; no newlines); no body.
- `kind=text` → raw UTF-8 in the body; an absent Content-Length means empty text
  dispatched immediately (`text:""`).
- `kind=clipboard` → no body.

### `/recv` labeled body (frozen order, colon-SPACE separator)

```
type: image|file|text
sender: <hostname>
path: <absolute path>     # present for image/file
text: <message>           # present for text; ALWAYS LAST; may contain newlines
```

`/last` and `/wait` return the bare absolute path with **no trailing newline** (the
`$(clipbeam last)` shell-substitution contract depends on this).

## Status codes

The **receive/control surface** emits only:
`200, 204, 400, 401, 403, 404, 405, 411, 413, 431, 500, 503`.

**The clipbeam server never emits `502` on the receive surface.** `502` is a
send-side mapping only (peer offline / ATS / local-network-denied). A clipbeam
*client* may still observe a `502` from something upstream (e.g. a reverse proxy in
front of a peer), which it maps to exit code 6.

Every non-2xx except 204 returns `application/json {"ok":false,"error":"<reason>"}`,
with the reason sanitized. A `500` surfaces the real errno (e.g. `ENOSPC`) as a
non-secret diagnostic.

## The CB01 SSH frame — internal only, NOT Envelope v1

> **`CB01` is an internal `clipbeam`↔`clipbeam` framing. It is NOT Envelope v1 and
> NEVER travels over `/clip`.**

On the daemonless SSH-exec fast path, `clipbeam send <file> user@host` streams a
raw-bytes, length-prefixed frame into a remote `clipbeam ingest`. It carries no
base64 (raw bytes, ~33% smaller) and exists purely so the two CLIs can stream
straight to disk with bounded memory. It is **deliberately kept out of the interop
fixtures** so no contributor mistakes it for the frozen wire.

```
"CB01"   4-byte magic
channel  1 byte    (0=clipboard, 1=agent)
count    uvarint   (>=1)
repeat count×:
  kind    1 byte   (0=image, 1=file, 2=text)
  name    uvarint-len + UTF-8 bytes
  uti     uvarint-len + UTF-8 bytes
  mime    uvarint-len + UTF-8 bytes
  payload uvarint-len + RAW bytes
```

**Cap semantics differ from `/clip`:** for a raw `CB01` frame, `raw == decoded`, so
the cap is the **decoded sum `<= maxBytes` directly**. The base64-inflated
`maxBytes*4/3 + 64KB` ceiling applies **only** to the base64-JSON `/clip` wire.
