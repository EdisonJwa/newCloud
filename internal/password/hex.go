package password

import "encoding/hex"

// decodeHex parses a hex-encoded hash, tolerating a trailing newline.
//
// A file written by hand or by `xxd` almost always ends with a newline, and rejecting
// that would make the break-glass path fail for a reason the operator cannot see from
// "invalid password".
func decodeHex(s string) ([]byte, error) {
	trimmed := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
			continue
		}
		trimmed = append(trimmed, c)
	}
	return hex.DecodeString(string(trimmed))
}
