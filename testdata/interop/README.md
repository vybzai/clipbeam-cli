# Interop golden fixtures (Envelope v1)

These are the **M1 interop gate** (PLAN §3.0, §12.5). They are **real bytes
captured from the shipped Swift app's frozen `Codable` types**, never
hand-authored from the spec — if they were hand-authored the gate would be
circular (the spec validating the spec).

## How they were captured

A throwaway Swift harness (run with `swift <file>` under `/tmp`, then deleted —
the committed app source in `~/Developer/clipbeam/Sources/ClipBeam/` was **not**
modified) copied the **frozen** `Item` / `Envelope` / `ClipResponse` /
`HealthResponse` / `AgentItem` types **verbatim** from `Models.swift` and:

- Encoded each envelope with `JSONEncoder().encode(env)` — the *exact* call
  `Sender.send` makes (`Sender.swift:61`). Plain encoder, **no `sortedKeys`**, so
  the wire key order is unspecified (only `config.json` uses `sortedKeys`).
- Reproduced the `/recv` labeled body via a verbatim copy of
  `Server.agentItemBody(_:)` (`Server.swift:766-771`).
- Reproduced `/health` and `ClipResponse` JSON via `JSONEncoder().encode(...)`,
  matching `Server.respondHealth` / `Server.dispatchClip`.

A second pass decoded every fixture back through the frozen types and asserted
the load-bearing semantics (empty-text present, channel nil vs set, PNG
signature, explicit-null tolerance) — all green.

The PNG payload is a real, decodable 2×2 RGBA PNG (77 bytes); the file payload
is 32 raw bytes. Both base64 strings are **unwrapped standard base64**
(`Data.base64EncodedString()`), matching the send-side lock in PLAN §3.6.

## How the Go tests must use them (decode-then-compare, NOT raw-byte equality)

PLAN §3.5 mandates **semantic** equality, never raw-byte equality. Two Swift
encoder quirks make raw-byte comparison wrong:

1. **Forward-slash escaping.** Swift's `JSONEncoder` escapes `/` as `\/` (seen in
   `image\/png`, the base64 `+`/`/` alphabet's `\/`, and every path in
   `saved`). Go's `encoding/json` emits a bare `/`. Both decode to the same
   string — compare *decoded values*.
2. **Unspecified key order.** Swift emits `text` before `kind`, and
   `saved`/`count`/`ok` in arbitrary order. Compare decoded structs/maps.

So the Go interop test decodes each fixture into `wire.Envelope` /
`wire.ClipResponse` / `wire.HealthResponse` and compares field-by-field; for the
Go **send** side it encodes its own envelope, decodes it back, and asserts
semantic equality with the matching fixture.

## Scrubbing (public repo)

The real `sender` host and absolute save paths were replaced before writing:

| Real (private)        | Scrubbed placeholder                          |
|-----------------------|-----------------------------------------------|
| sender hostname       | `macbook.example`                             |
| clipboard save dir    | `/home/agent/.local/share/clipbeam`           |
| agent inbox dir       | `/home/agent/.local/state/clipbeam/agent-inbox` |

`/health` keeps the Swift app's *values* (`app:"ClipBeam"`, `version:"1.0.0"`,
`platform:"macOS 26.0.0"`, scrubbed `host`) because only the **key names** are
frozen. The Go CLI emits its **own** values for the same keys
(`app:"clipbeam"`, `platform:"linux <kernel>"`, its own `version`) — see PLAN
§3.3. The Go `/health` test asserts key presence + types, not these values.

## Fixture catalogue

### Envelopes (the `/clip` wire body — what `Sender.send` POSTs)

| File | What it proves |
|---|---|
| `envelope_image_png.json` | PNG image item; `name`/`uti`/`mime`/`bytesB64` present; **`channel` key absent** (nil → omitted). |
| `envelope_file.json` | File item (`kind:"file"`, `public.data` / `application/octet-stream`). |
| `envelope_text.json` | Text item: `text` present, **no** `bytesB64`, `name`/`uti`/`mime` omitted. |
| `envelope_text_empty.json` | **The `&""` rule.** `text:""` is **emitted** (`{"text":"","kind":"text"}`) — present-empty, NOT dropped. An empty `/agent-send` body produces exactly this. A plain `string,omitempty` in Go would wrongly drop it. |
| `envelope_multi.json` | Multi-item envelope: image + file + text in one envelope (item order preserved). |
| `envelope_agent_text.json` | Agent channel: `"channel":"agent"` explicit; text-only. |
| `envelope_agent_image.json` | Agent channel image (yields a path-bearing agent inbox item on `/recv`). |
| `envelope_channel_omitted.json` | **`channel` key absent** → clipboard behavior (nil == clipboard, `Models.swift:25-30`). |
| `envelope_channel_clipboard.json` | `"channel":"clipboard"` explicit — decodes equal in *behavior* to the omitted form; proves both forms are accepted. |

### `/clip` `ClipResponse` bodies (the 200 reply)

| File | What it proves |
|---|---|
| `response_clip_image.json` | `{ok,saved:[path],count:1}` for a single saved image. |
| `response_clip_multi.json` | Three saved paths incl. a `clipbeam-<UTC>.txt` text sidecar; `count:3`. |
| `response_clip_agent_text.json` | Agent text-only receive: `saved:[]` (agent text is never written to disk) but `count:1`. |
| `response_clip_agent_image.json` | Agent image receive: `saved:[inbox/...png]`, `count:1`. |

### `/health` body

| File | What it proves |
|---|---|
| `response_health.json` | Frozen key names `ok,app,version,host,platform`. Values are the Swift app's (scrubbed host); the Go CLI substitutes its own values for the same keys. |

### `/recv` labeled text bodies (`agentItemBody`, `text/plain; charset=utf-8`)

| File | What it proves |
|---|---|
| `recv_text.txt` | `type: …\nsender: …\ntext: …` — colon-SPACE, no `path:` for text, **`text:` last**, **no trailing newline**. |
| `recv_image.txt` | `type: …\nsender: …\npath: …` — `path:` present for image, **no** `text:`. |
| `recv_text_multiline.txt` | `text:` last carries embedded `\n`s — anything after the `text:` label is unambiguously the text. |

### `/last` body

| File | What it proves |
|---|---|
| `last_path.txt` | A bare absolute path with **no trailing newline** (the `$(clipbeam last)` substitution depends on it). |

## Re-capturing

Rebuild the throwaway harness from this README + `Models.swift`/`Server.swift`,
re-run, re-scrub, re-verify the Swift round-trip. Do **not** edit these JSON/txt
files by hand — regenerate them so they stay real captures.
