package sshx

import "github.com/vybzai/clipbeam-cli/internal/ingest"

// Magic is the CB01 frame magic, exported so the decoder/tests can assert it.
const Magic = cb01Magic

// ChannelByte collapses a wire channel string ("" | "clipboard" | "agent") to its
// CB01 channel byte (the nil/"clipboard" distinction collapses to 0, PLAN §5.1).
func ChannelByte(channel string) byte {
	if channel == ingest.ChannelAgent {
		return cb01ChannelAgent
	}
	return cb01ChannelClipboard
}

// ChannelString maps a CB01 channel byte back to its wire channel string.
func ChannelString(b byte) string {
	if b == cb01ChannelAgent {
		return ingest.ChannelAgent
	}
	return ingest.ChannelClipboard
}

// KindByte maps an ingest kind string to its CB01 kind byte.
func KindByte(kind string) byte {
	switch kind {
	case ingest.KindFile:
		return cb01KindFile
	case ingest.KindText:
		return cb01KindText
	default:
		return cb01KindImage
	}
}

// KindString maps a CB01 kind byte back to its ingest kind string.
func KindString(b byte) string {
	switch b {
	case cb01KindFile:
		return ingest.KindFile
	case cb01KindText:
		return ingest.KindText
	default:
		return ingest.KindImage
	}
}
