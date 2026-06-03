// Package auth provides the hand-rolled constant-time token compare and the CSPRNG
// shared-token generator. Both are byte-for-behavior ports of the Swift app
// (Server.swift constantTimeEquals + Keychain.swift makeToken, PLAN §3.12) so a
// Mac↔Linux pair shares one 120-bit secret.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
)

// base32Alphabet is RFC 4648 Base32, no padding. 15 CSPRNG bytes (120 bits) encode
// to exactly 24 chars (PLAN §3.12, Keychain.swift base32Encode).
const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// tokenRandomBytes is the CSPRNG byte count: 15 bytes = 120 bits → 24 Base32 chars.
const tokenRandomBytes = 15

// TokenChars is the exact length of a generated token (24).
const TokenChars = 24

// ConstantTimeEquals reports whether the presented token equals the expected token
// WITHOUT leaking the token length or a matching prefix via timing.
//
// crypto/subtle.ConstantTimeCompare returns 0 immediately on a length mismatch, so
// it cannot by itself satisfy the no-length-leak property (PLAN §3.12). This wrapper
// hand-rolls Swift's constantTimeEquals: a nil/missing token still burns a loop;
// unequal lengths still iterate max(len) folding a non-zero length-difference bit;
// subtle.ConstantTimeCompare is used only on the equal-length branch.
//
// presented is a pointer so a missing header (nil) is distinguished from an empty
// string ("") — both return false but a nil still burns the loop.
func ConstantTimeEquals(presented *string, expected string) bool {
	exp := []byte(expected)
	if presented == nil {
		// Burn a fixed-time pass over expected so a nil token costs the same as a
		// wrong one: fold every byte into an accumulator (data-dependent, not cheaply
		// foldable). The function returns false unconditionally regardless of acc.
		var acc byte
		for _, x := range exp {
			acc |= x
		}
		_ = acc
		return false
	}
	pres := []byte(*presented)

	// Fold a non-zero length-difference bit into diff so a mismatch can never be
	// equal, while still iterating a fixed number of times (max of the two lengths).
	var diff byte
	if len(pres) != len(exp) {
		diff = 1
	}
	n := len(pres)
	if len(exp) > n {
		n = len(exp)
	}
	if n == 0 {
		return diff == 0
	}
	for i := 0; i < n; i++ {
		var x, y byte
		if i < len(pres) {
			x = pres[i]
		}
		if i < len(exp) {
			y = exp[i]
		}
		diff |= x ^ y
	}
	if len(pres) == len(exp) {
		// Equal-length branch: also run the vetted constant-time primitive.
		return subtle.ConstantTimeCompare(pres, exp) == 1 && diff == 0
	}
	return diff == 0
}

// MakeToken generates a fresh 120-bit shared token: 15 CSPRNG bytes via crypto/rand
// → RFC 4648 Base32 (no padding) → exactly 24 chars. NEVER math/rand (PLAN §3.12).
// It returns an error only on a short read from the system CSPRNG.
func MakeToken() (string, error) {
	b := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32Encode(b), nil
}

// base32Encode encodes bytes as RFC 4648 Base32 with no padding (Keychain.swift
// base32Encode). 15 bytes → 24 chars.
func base32Encode(bytes []byte) string {
	out := make([]byte, 0, (len(bytes)*8+4)/5)
	var buffer uint32
	var bitsInBuffer int
	for _, b := range bytes {
		buffer = (buffer << 8) | uint32(b)
		bitsInBuffer += 8
		for bitsInBuffer >= 5 {
			bitsInBuffer -= 5
			idx := (buffer >> uint32(bitsInBuffer)) & 0x1F
			out = append(out, base32Alphabet[idx])
		}
	}
	if bitsInBuffer > 0 {
		idx := (buffer << uint32(5-bitsInBuffer)) & 0x1F
		out = append(out, base32Alphabet[idx])
	}
	return string(out)
}
