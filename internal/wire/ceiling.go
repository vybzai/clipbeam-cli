package wire

// DefaultMaxBytes is the default DECODED-payload cap: 52_428_800 = 50 MB
// (Swift Config.defaults.maxBytes, PLAN §3.8).
const DefaultMaxBytes = 52_428_800

// HardCeiling returns the raw-wire byte ceiling for the base64-JSON /clip wire,
// derived from the DECODED cap. base64 inflates by ~33%, plus 64 KB header slack.
//
// The exact formula is `maxBytes*4/3 + 64*1024` with Go INTEGER division and
// MULTIPLY-FIRST — `maxBytes/3*4` or float math would change the truncation and
// accept/reject a payload the Mac app would reject/accept. For the default
// maxBytes (52428800) this is exactly 69970602 (PLAN §3.8; a unit test asserts the
// literal). This ceiling applies ONLY to the base64-JSON /clip wire — the raw-bytes
// CB01 SSH frame caps decoded-sum ≤ maxBytes directly (raw==decoded, PLAN §5.1).
func HardCeiling(maxBytes int) int {
	return maxBytes*4/3 + 64*1024
}

// HardCeilingHeaderBytes is the HTTP header-section ceiling that maps to 431
// (mirrors Swift's 64 KB header cap, PLAN §3.3).
const HardCeilingHeaderBytes = 64 * 1024

// DecodedLength returns the decoded byte length of a base64 string without
// allocating the decoded buffer — a cheap sender pre-flight that replicates Swift
// Sender.swift decodedLength(ofBase64:). It counts non-whitespace, non-pad chars,
// counts '=' padding separately, then groups=(chars+pad)/4, decoded=groups*3-pad,
// clamped ≥ 0 (PLAN §3.8).
func DecodedLength(b64 string) int {
	chars := 0
	pad := 0
	for i := 0; i < len(b64); i++ {
		switch b64[i] {
		case '=':
			pad++
		case ' ', '\t', '\n', '\r':
			// whitespace — ignored by decoders
			continue
		default:
			chars++
		}
	}
	groups := (chars + pad) / 4
	decoded := groups*3 - pad
	if decoded < 0 {
		return 0
	}
	return decoded
}
