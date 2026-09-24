package pgwire

import (
	"strings"
	"unicode/utf8"
)

// saslPrep prepares a password for SCRAM the way PostgreSQL does (RFC 4013,
// SASLprep), so a password with a soft hyphen or a no-break space in it
// authenticates here just as it does from psql.
//
// The server stores the prepared form of a password when the password can be
// prepared, and the raw bytes when it cannot; the client must pick the same
// one. This follows the server's steps: an ASCII password is used as is; a
// non-ASCII one has its non-ASCII spaces mapped to a plain space and the
// characters "commonly mapped to nothing" removed; and a password that is not
// valid UTF-8, that prepares to nothing, or that still holds a prohibited
// character falls back to the raw bytes.
//
// One step is left out: Unicode NFKC normalization, which needs tables the
// standard library does not carry. A password that normalization would change
// - compatibility forms such as fullwidth letters or ligatures, or a letter
// followed by a combining accent - will not authenticate; one already in
// normal form, the usual case, does.
func saslPrep(password string) string {
	ascii := true
	for i := 0; i < len(password); i++ {
		if password[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii || !utf8.ValidString(password) {
		return password
	}

	var b strings.Builder
	for _, r := range password {
		switch {
		case inRanges(r, nonASCIISpace):
			b.WriteByte(' ')
		case inRanges(r, mappedToNothing):
		default:
			b.WriteRune(r)
		}
	}
	prepared := b.String()
	if prepared == "" {
		return password
	}
	for _, r := range prepared {
		if inRanges(r, prohibited) || isNonCharacter(r) {
			return password
		}
	}
	return prepared
}

// runeRange is an inclusive range of code points.
type runeRange struct{ lo, hi rune }

func inRanges(r rune, table []runeRange) bool {
	for _, rr := range table {
		if r >= rr.lo && r <= rr.hi {
			return true
		}
	}
	return false
}

// nonASCIISpace is RFC 3454 table C.1.2, mapped to SPACE.
var nonASCIISpace = []runeRange{
	{0x00A0, 0x00A0}, {0x1680, 0x1680}, {0x2000, 0x200B}, {0x202F, 0x202F},
	{0x205F, 0x205F}, {0x3000, 0x3000},
}

// mappedToNothing is RFC 3454 table B.1, removed.
var mappedToNothing = []runeRange{
	{0x00AD, 0x00AD}, {0x034F, 0x034F}, {0x1806, 0x1806}, {0x180B, 0x180D},
	{0x200B, 0x200D}, {0x2060, 0x2060}, {0xFE00, 0xFE0F}, {0xFEFF, 0xFEFF},
}

// prohibited is the output SASLprep refuses: RFC 3454 tables C.2 through C.9
// (control characters, private use, surrogates, characters inappropriate for
// plain text or canonical representation, change-display and tagging
// characters). Non-characters (C.4) are checked by isNonCharacter.
var prohibited = []runeRange{
	{0x0000, 0x001F}, {0x007F, 0x009F}, {0x0340, 0x0341}, {0x06DD, 0x06DD},
	{0x070F, 0x070F}, {0x180E, 0x180E}, {0x200C, 0x200F}, {0x2028, 0x202E},
	{0x2060, 0x2063}, {0x206A, 0x206F}, {0x2FF0, 0x2FFB}, {0xD800, 0xDFFF},
	{0xE000, 0xF8FF}, {0xFDD0, 0xFDEF}, {0xFEFF, 0xFEFF}, {0xFFF9, 0xFFFD},
	{0x1D173, 0x1D17A}, {0xE0001, 0xE0001}, {0xE0020, 0xE007F},
	{0xF0000, 0xFFFFD}, {0x100000, 0x10FFFD},
}

// isNonCharacter reports the code points ending in FFFE or FFFF in every
// plane.
func isNonCharacter(r rune) bool {
	return r&0xFFFE == 0xFFFE
}
