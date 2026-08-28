package settings

import (
	"fmt"
	"strings"
)

// secretMarkers make a value unprintable when one of them appears anywhere in
// the upper-cased variable name.
//
// The name is the whole test: there is no list of known-secret variables and no
// flag stored beside the value, because both are things a user can forget to
// set, and the cost of forgetting is a key in a screenshot, a bug report or a
// recorded screen share — after which the key has to be rotated, not un-seen.
//
// PASSWD is listed separately from PASSWORD on purpose. It is not a substring of
// it, and the Unix spelling is the one people actually type.
var secretMarkers = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"}

// IsSecret reports whether a variable's NAME means its value must never be
// displayed.
func IsSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range secretMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// Redact renders a value for display. Non-secret names pass through unchanged.
//
// A secret shows at most its first four characters, which is enough to tell two
// keys apart and to confirm a paste landed, and never the tail: the tail is the
// part that identifies the account, and a key with its middle hidden but its end
// shown is still a key that has to be rotated once it has been on screen.
func Redact(name, value string) string {
	if !IsSecret(name) {
		return value
	}
	// Runes, not bytes. Slicing a CJK value at byte 4 cuts a character in half
	// and prints a replacement glyph, so the operator cannot tell whether the
	// value is the one they pasted — and a byte count would report three times
	// the number of characters they typed.
	runes := []rune(value)
	switch n := len(runes); {
	case n == 0:
		return "(unset)"
	case n <= 8:
		// Four characters out of eight is half the secret. Short values get a
		// length and nothing else.
		return fmt.Sprintf("(set, %d chars)", n)
	default:
		return fmt.Sprintf("%s… (%d chars)", string(runes[:4]), n)
	}
}
