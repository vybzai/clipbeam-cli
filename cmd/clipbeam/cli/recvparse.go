package cli

import "strings"

// recvItem is the parsed form of the labeled GET /recv body (PLAN §8.2). type and
// sender are always present; path is set for image/file; text is set for text and may
// span multiple lines. cid is the opt-in agent-channel correlation token extracted
// from a leading [clipbeam:cid=<uuid>] in the text body (PLAN §8.6; default off — when
// the convention is not present, Cid stays "").
type recvItem struct {
	Type   string
	Sender string
	Path   string // "" when absent
	Text   string // "" when absent (the empty-text item is text:"")
	HasText bool  // distinguishes an absent text: line from a present-but-empty one
	Cid    string // extracted [clipbeam:cid=…] token, or "" (PLAN §8.6)
}

// parseRecvBody parses the labeled /recv body emitted by writeRecvBody / Swift
// Server.swift:767-769 (PLAN §8.2 parse note): lines are "type", "sender", optional
// "path", and (LAST) "text" which consumes everything after "text: " verbatim —
// embedded newlines preserved. Each label uses a literal colon-SPACE separator; the
// CLI splits on the FIRST colon and drops the single leading space, so the path/text
// values are never polluted by a leading space. Because text is always last and may
// contain its own newlines, once the "text: " label is seen the remainder of the body
// (after the single leading space) is the text verbatim.
func parseRecvBody(body string) recvItem {
	var it recvItem
	rest := body
	for {
		// Find the next line boundary.
		nl := strings.IndexByte(rest, '\n')
		var line string
		if nl < 0 {
			line = rest
			rest = ""
		} else {
			line = rest[:nl]
			rest = rest[nl+1:]
		}

		label, value := splitFirstColon(line)
		switch label {
		case "type":
			it.Type = value
		case "sender":
			it.Sender = value
		case "path":
			it.Path = value
		case "text":
			// text is LAST and consumes the rest of the body VERBATIM (embedded
			// newlines preserved). value is the first physical line after "text: ";
			// append the unparsed remainder (which still carries its own newlines).
			it.HasText = true
			if rest != "" {
				it.Text = value + "\n" + rest
			} else {
				it.Text = value
			}
			rest = "" // consumed
		}
		if rest == "" {
			break
		}
	}
	if it.HasText {
		it.Text, it.Cid = extractCid(it.Text)
	}
	return it
}

// splitFirstColon splits a labeled line on the FIRST colon and drops a single leading
// space from the value (PLAN §8.2): "type: image" → ("type","image"); "text: a:b" →
// ("text","a:b") — only the first colon is the separator, so a value containing colons
// (a path, a URL) is preserved. A line with no colon yields ("", line).
func splitFirstColon(line string) (label, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", line
	}
	label = line[:i]
	value = line[i+1:]
	// Drop exactly ONE leading space (the literal ": " colon-SPACE separator). A value
	// that legitimately begins with two spaces keeps the second.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return label, value
}

// cidPrefix is the opt-in agent-channel correlation token (PLAN §8.6): a leading
// "[clipbeam:cid=<uuid>] " prepended to the text body by `clipbeam msg --reply-to`.
const cidOpen = "[clipbeam:cid="
const cidClose = "]"

// extractCid pulls a leading [clipbeam:cid=<uuid>] token off the text body if present,
// returning the de-tokenized text and the cid (PLAN §8.6). When the convention is not
// present the text is returned unchanged and cid is "". A single space immediately
// after the token is consumed so a Swift peer that never emits the token round-trips
// cleanly (it simply never matches here).
func extractCid(text string) (clean, cid string) {
	if !strings.HasPrefix(text, cidOpen) {
		return text, ""
	}
	end := strings.Index(text, cidClose)
	if end < 0 {
		return text, ""
	}
	cid = text[len(cidOpen):end]
	clean = text[end+len(cidClose):]
	// Consume exactly one separating space after the token, if present.
	clean = strings.TrimPrefix(clean, " ")
	return clean, cid
}

// prependCid builds a text body with a leading [clipbeam:cid=<uuid>] token (PLAN §8.6),
// used by `clipbeam msg --reply-to <cid>`. The token is followed by a single space then
// the message text.
func prependCid(cid, text string) string {
	return cidOpen + cid + cidClose + " " + text
}
