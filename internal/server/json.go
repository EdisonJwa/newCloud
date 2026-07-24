package server

import (
	"encoding/json"
	"io"
)

// jsonDecode is a thin wrapper so handlers do not import encoding/json directly
// and the disallow-unknown-fields policy is applied in one place.
//
// Rejecting unknown fields matters: a client that misspells a security-relevant
// field (say "permisions") should get an error, not a silently weaker share.
func jsonDecode(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
